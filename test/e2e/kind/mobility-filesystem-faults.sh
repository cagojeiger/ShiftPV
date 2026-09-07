#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${WORK_DIR:?WORK_DIR is required}"
: "${WORKER_A_POOL:?WORKER_A_POOL is required}"
: "${WORKER_B_POOL:?WORKER_B_POOL is required}"

SOURCE_NODE="${CLUSTER_NAME}-worker"
DESTINATION_NODE="${CLUSTER_NAME}-worker2"
DESTINATION_MOUNT=/srv/shiftpv-b
MOUNT_STATE=normal

restore_destination_mount() {
	case "${MOUNT_STATE}" in
	tmpfs_ro)
		docker exec "${DESTINATION_NODE}" mount -o remount,rw "${DESTINATION_MOUNT}" >/dev/null 2>&1 || true
		docker exec "${DESTINATION_NODE}" umount "${DESTINATION_MOUNT}" >/dev/null 2>&1 || true
		;;
	tmpfs_rw | enospc)
		docker exec "${DESTINATION_NODE}" umount "${DESTINATION_MOUNT}" >/dev/null 2>&1 || true
		;;
	esac
	MOUNT_STATE=normal
	kubectl uncordon "${SOURCE_NODE}" >/dev/null 2>&1 || true
	kubectl uncordon "${DESTINATION_NODE}" >/dev/null 2>&1 || true
	kubectl -n shiftpv-system scale deployment/shiftpv-controller --replicas=1 >/dev/null 2>&1 || true
}
trap restore_destination_mount EXIT

controller_down() {
	kubectl -n shiftpv-system scale deployment/shiftpv-controller --replicas=0
	local deadline=$((SECONDS + 120))
	while ((SECONDS < deadline)); do
		if [[ -z "$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o name 2>/dev/null)" ]]; then
			return
		fi
		sleep 1
	done
	echo 'controller Pod did not stop' >&2
	return 1
}

controller_up() {
	kubectl -n shiftpv-system scale deployment/shiftpv-controller --replicas=1
	kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=180s
}

render_workload() {
	local namespace=$1 payload=$2
	sed \
		-e "s|__NAMESPACE__|${namespace}|g" \
		-e "s|__PAYLOAD__|${payload}|g" \
		"${ROOT_DIR}/test/e2e/kind/mobility/manifests/filesystem-fault-workload.yaml.tpl" \
		>"${WORK_DIR}/${namespace}.yaml"
}

create_source_workload() {
	local namespace=$1 payload=$2
	kubectl uncordon "${SOURCE_NODE}" >/dev/null 2>&1 || true
	kubectl cordon "${DESTINATION_NODE}"
	render_workload "${namespace}" "${payload}"
	kubectl apply -f "${WORK_DIR}/${namespace}.yaml"
	kubectl -n "${namespace}" rollout status deployment/writer --timeout=180s
	kubectl -n "${namespace}" wait --for=jsonpath='{.status.phase}'=Bound pvc/data --timeout=120s

	PVC_UID=$(kubectl -n "${namespace}" get pvc/data -o jsonpath='{.metadata.uid}')
	PV_NAME=$(kubectl -n "${namespace}" get pvc/data -o jsonpath='{.spec.volumeName}')
	VOLUME_ID=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
	SOURCE_POD=$(kubectl -n "${namespace}" get pod -l "app=${namespace}" -o jsonpath='{.items[0].metadata.name}')
	test "$(kubectl -n "${namespace}" get "pod/${SOURCE_POD}" -o jsonpath='{.spec.nodeName}')" = "${SOURCE_NODE}"
	SOURCE_CHECKSUM=$(kubectl -n "${namespace}" exec "${SOURCE_POD}" -- sha256sum /data/payload | awk '{print $1}')
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
	kubectl uncordon "${DESTINATION_NODE}"
}

wait_for_move() {
	local deadline=$((SECONDS + 120))
	MOVE_NAME=""
	while ((SECONDS < deadline)); do
		MOVE_NAME=$(kubectl get shiftpvmoves -o jsonpath="{.items[?(@.spec.volumeID=='${VOLUME_ID}')].metadata.name}" 2>/dev/null || true)
		[[ -n "${MOVE_NAME}" ]] && return
		sleep 0.2
	done
	echo "Move was not created for ${VOLUME_ID}" >&2
	return 1
}

wait_for_deferred_discovery() {
	local deadline=$((SECONDS + 120))
	while ((SECONDS < deadline)); do
		if kubectl -n shiftpv-system logs deployment/shiftpv-controller -c shiftpv-controller --since=2m 2>/dev/null |
			grep -Fq "deferred ShiftPV mobility for volume ${VOLUME_ID}: NoCompatibleDestination"; then
			return
		fi
		sleep 1
	done
	echo "mobility discovery was not observably deferred for ${VOLUME_ID}" >&2
	kubectl -n shiftpv-system logs deployment/shiftpv-controller -c shiftpv-controller --since=2m >&2 || true
	return 1
}

wait_for_pool_condition() {
	local pool=$1 status=$2 reason=$3 deadline=$((SECONDS + 120))
	local actual_status="" actual_reason=""
	while ((SECONDS < deadline)); do
		actual_status=$(kubectl get "shiftpvpool/${pool}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
		actual_reason=$(kubectl get "shiftpvpool/${pool}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || true)
		[[ "${actual_status}" == "${status}" && "${actual_reason}" == "${reason}" ]] && return
		sleep 1
	done
	echo "Pool condition timeout: pool=${pool} expected=${status}/${reason} actual=${actual_status}/${actual_reason}" >&2
	kubectl get "shiftpvpool/${pool}" -o yaml >&2 || true
	return 1
}

wait_for_move_state() {
	local phase=$1 reason=$2 deadline=$((SECONDS + 300))
	local actual_phase="" actual_reason=""
	while ((SECONDS < deadline)); do
		actual_phase=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		actual_reason=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.reason}' 2>/dev/null || true)
		[[ "${actual_phase}" == "${phase}" && "${actual_reason}" == "${reason}" ]] && return
		sleep 1
	done
	echo "Move state timeout: move=${MOVE_NAME} expected=${phase}/${reason} actual=${actual_phase}/${actual_reason}" >&2
	kubectl get "shiftpvmove/${MOVE_NAME}" "shiftpvvolume/${VOLUME_ID}" -o yaml >&2 || true
	return 1
}

wait_for_success() {
	local namespace=$1
	kubectl wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.phase}'=Succeeded --timeout=300s
	kubectl -n "${namespace}" rollout status deployment/writer --timeout=180s
	local pod checksum
	pod=$(kubectl -n "${namespace}" get pod -l "app=${namespace}" -o jsonpath='{.items[0].metadata.name}')
	test "$(kubectl -n "${namespace}" get "pod/${pod}" -o jsonpath='{.spec.nodeName}')" = "${DESTINATION_NODE}"
	checksum=$(kubectl -n "${namespace}" exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')
	test "${checksum}" = "${SOURCE_CHECKSUM}"
	test "$(kubectl -n "${namespace}" get pvc/data -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
	test "$(kubectl -n "${namespace}" get pvc/data -o jsonpath='{.spec.volumeName}')" = "${PV_NAME}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = Ready
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${DESTINATION_NODE}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = ""
	docker exec "${DESTINATION_NODE}" test -f "${DESTINATION_MOUNT}/volumes/${VOLUME_ID}/payload"
	test ! -e "${WORKER_A_POOL}/volumes/${VOLUME_ID}"
	test ! -e "${WORKER_A_POOL}/.shiftpv/retired/${MOVE_NAME}"
}

delete_workload() {
	local namespace=$1
	kubectl delete "namespace/${namespace}" --wait=true --timeout=180s
	kubectl uncordon "${SOURCE_NODE}" >/dev/null 2>&1 || true
}

# Give the harness a deterministic pause between Move discovery and disk work.
# The product remains restart-safe; only the test interval is widened.
helm upgrade shiftpv "${ROOT_DIR}/charts/shiftpv" \
	--namespace shiftpv-system \
	--reuse-values \
	--set mobility.interval=10s \
	--wait \
	--timeout 5m

# A full destination must be removed from candidate selection before rsync
# creates staging data or changes volume authority. Once the same registered
# mount becomes writable again, readiness and mobility must resume without an
# operator recovery request.
ENOSPC_NAMESPACE=shiftpv-mobility-enospc
create_source_workload "${ENOSPC_NAMESPACE}" 'ShiftPV mobility ENOSPC recovery'
docker exec "${DESTINATION_NODE}" mount -t tmpfs -o size=1m,nr_inodes=128 shiftpv-mobility-enospc "${DESTINATION_MOUNT}"
MOUNT_STATE=tmpfs_rw
docker exec "${DESTINATION_NODE}" sh -c "dd if=/dev/zero of='${DESTINATION_MOUNT}/capacity-fill' bs=1M count=2 >/dev/null 2>&1 || true"
wait_for_pool_condition worker-b False NoSpace
kubectl cordon "${SOURCE_NODE}"
wait_for_deferred_discovery
test -z "$(kubectl get shiftpvmoves -o jsonpath="{.items[?(@.spec.volumeID=='${VOLUME_ID}')].metadata.name}" 2>/dev/null || true)"
docker exec "${DESTINATION_NODE}" test ! -e "${DESTINATION_MOUNT}/.shiftpv/incoming"
docker exec "${DESTINATION_NODE}" umount "${DESTINATION_MOUNT}"
MOUNT_STATE=normal
wait_for_pool_condition worker-b True PoolReady
wait_for_move
wait_for_success "${ENOSPC_NAMESPACE}"
echo "mobility resumed after destination ENOSPC recovery: volume=${VOLUME_ID} move=${MOVE_NAME}"
delete_workload "${ENOSPC_NAMESPACE}"

# Pause the controller after a verified copy, remount the same destination
# filesystem read-only, and prove promotion pauses before owner commit. Once the
# mount is writable again, the existing transaction must finish automatically.
READONLY_NAMESPACE=shiftpv-mobility-readonly
create_source_workload "${READONLY_NAMESPACE}" 'ShiftPV mobility read-only recovery'
docker exec "${DESTINATION_NODE}" mount -t tmpfs -o size=8m,nr_inodes=1024 shiftpv-mobility-readonly "${DESTINATION_MOUNT}"
MOUNT_STATE=tmpfs_rw
kubectl cordon "${SOURCE_NODE}"
wait_for_move
COPY_JOB=""
COPY_JOB_DEADLINE=$((SECONDS + 180))
while ((SECONDS < COPY_JOB_DEADLINE)); do
	COPY_JOB=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.copyJobName}' 2>/dev/null || true)
	if [[ -n "${COPY_JOB}" ]] && kubectl -n shiftpv-system get "job/${COPY_JOB}" >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done
if [[ -z "${COPY_JOB}" ]] || ! kubectl -n shiftpv-system get "job/${COPY_JOB}" >/dev/null 2>&1; then
	echo "copy Job was not created before the read-only test deadline: move=${MOVE_NAME} copyJobName=${COPY_JOB}" >&2
	kubectl get "shiftpvmove/${MOVE_NAME}" -o yaml >&2 || true
	exit 1
fi
controller_down
kubectl -n shiftpv-system wait "job/${COPY_JOB}" --for=condition=complete --timeout=180s
docker exec "${DESTINATION_NODE}" test -f "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}/.shiftpv-move-id"
docker exec "${DESTINATION_NODE}" test -f "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}/payload"
docker exec "${DESTINATION_NODE}" mount -o remount,ro "${DESTINATION_MOUNT}"
MOUNT_STATE=tmpfs_ro
if docker exec "${DESTINATION_NODE}" touch "${DESTINATION_MOUNT}/readonly-probe" 2>/dev/null; then
	echo 'destination remount did not become read-only' >&2
	exit 1
fi
wait_for_pool_condition worker-b False ReadOnly
controller_up
wait_for_move_state Copying DestinationUnavailable
test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
docker exec "${DESTINATION_NODE}" test -f "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}/.shiftpv-move-id"
docker exec "${DESTINATION_NODE}" test ! -e "${DESTINATION_MOUNT}/volumes/${VOLUME_ID}"
docker exec "${DESTINATION_NODE}" mount -o remount,rw "${DESTINATION_MOUNT}"
MOUNT_STATE=tmpfs_rw
wait_for_pool_condition worker-b True PoolReady
wait_for_success "${READONLY_NAMESPACE}"
docker exec "${DESTINATION_NODE}" test ! -e "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}"
echo "mobility resumed after destination read-only recovery: volume=${VOLUME_ID} move=${MOVE_NAME}"
delete_workload "${READONLY_NAMESPACE}"
docker exec "${DESTINATION_NODE}" umount "${DESTINATION_MOUNT}"
MOUNT_STATE=normal

echo 'ShiftPV mobility filesystem fault recovery passed'
