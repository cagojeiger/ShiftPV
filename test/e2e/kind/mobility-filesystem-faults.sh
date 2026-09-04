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

wait_for_blocked() {
	local reason=$1
	kubectl wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.phase}'=Blocked --timeout=300s
	local actual_reason volume_phase owner active_move
	actual_reason=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.reason}')
	volume_phase=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')
	owner=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')
	active_move=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')
	if [[ "${actual_reason}" != "${reason}" || "${volume_phase}" != Blocked || "${owner}" != "${SOURCE_NODE}" || "${active_move}" != "${MOVE_NAME}" || ! -f "${WORKER_A_POOL}/volumes/${VOLUME_ID}/payload" || -e "${WORKER_B_POOL}/volumes/${VOLUME_ID}" ]]; then
		echo "blocked-state mismatch: expected_reason=${reason} actual_reason=${actual_reason} volume_phase=${volume_phase} owner=${owner} active_move=${active_move}" >&2
		kubectl get "shiftpvmove/${MOVE_NAME}" "shiftpvvolume/${VOLUME_ID}" -o yaml >&2 || true
		return 1
	fi
}

request_source_recovery() {
	local namespace=$1
	kubectl uncordon "${SOURCE_NODE}"
	kubectl patch "shiftpvmove/${MOVE_NAME}" --type merge -p '{"spec":{"recovery":"ResumeOwner"}}'
	kubectl wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.recoveryPhase}'=Recovered --timeout=300s
	kubectl -n "${namespace}" rollout status deployment/writer --timeout=180s
	local pod checksum
	pod=$(kubectl -n "${namespace}" get pod -l "app=${namespace}" -o jsonpath='{.items[0].metadata.name}')
	test "$(kubectl -n "${namespace}" get "pod/${pod}" -o jsonpath='{.spec.nodeName}')" = "${SOURCE_NODE}"
	checksum=$(kubectl -n "${namespace}" exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')
	test "${checksum}" = "${SOURCE_CHECKSUM}"
	test "$(kubectl -n "${namespace}" get pvc/data -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
	test "$(kubectl -n "${namespace}" get pvc/data -o jsonpath='{.spec.volumeName}')" = "${PV_NAME}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = Ready
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = ""
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

# A destination can contain partial staging data when rsync reaches ENOSPC.
# Recovery must keep the source authoritative and quarantine, never promote,
# whatever the failed copy left behind.
ENOSPC_NAMESPACE=shiftpv-mobility-enospc
create_source_workload "${ENOSPC_NAMESPACE}" 'ShiftPV mobility ENOSPC recovery'
docker exec "${DESTINATION_NODE}" mount -t tmpfs -o size=1m,nr_inodes=128 shiftpv-mobility-enospc "${DESTINATION_MOUNT}"
MOUNT_STATE=tmpfs_rw
kubectl cordon "${SOURCE_NODE}"
wait_for_move
controller_down
test -z "$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.copyJobName}')"
docker exec "${DESTINATION_NODE}" sh -ec '
  staging="/srv/shiftpv-b/.shiftpv/incoming/'"${MOVE_NAME}"'"
  mkdir -p "${staging}"
  printf "pre-existing partial staging\n" > "${staging}/partial-before-rsync"
  if dd if=/dev/zero of=/srv/shiftpv-b/enospc-fill bs=1M count=2 2>/dev/null; then
    echo "tmpfs size limit was not enforced" >&2
    exit 1
  fi
  test -s /srv/shiftpv-b/enospc-fill
'
MOUNT_STATE=enospc
controller_up
wait_for_blocked CopyFailed
echo 'checking ENOSPC staging directory'
docker exec "${DESTINATION_NODE}" test -d "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}"
echo 'checking ENOSPC staging marker is not valid'
STAGING_MARKER=$(docker exec "${DESTINATION_NODE}" sh -c 'if test -f "$1"; then cat "$1"; fi' sh "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}/.shiftpv-move-id")
test "${STAGING_MARKER}" != "${MOVE_NAME}"
echo 'checking ENOSPC partial staging payload'
PARTIAL_ENTRY=$(docker exec "${DESTINATION_NODE}" find "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}" -mindepth 1 -print -quit)
test -n "${PARTIAL_ENTRY}"
echo "observed partial staging entry: ${PARTIAL_ENTRY}"
echo 'freeing ENOSPC filler'
docker exec "${DESTINATION_NODE}" rm -f "${DESTINATION_MOUNT}/enospc-fill"
MOUNT_STATE=tmpfs_rw
echo 'requesting source recovery after ENOSPC'
request_source_recovery "${ENOSPC_NAMESPACE}"
docker exec "${DESTINATION_NODE}" test -d "${DESTINATION_MOUNT}/.shiftpv/aborted/${MOVE_NAME}-incoming"
docker exec "${DESTINATION_NODE}" test ! -e "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}"
echo "mobility ENOSPC partial-staging recovery passed: volume=${VOLUME_ID} move=${MOVE_NAME}"
delete_workload "${ENOSPC_NAMESPACE}"
docker exec "${DESTINATION_NODE}" umount "${DESTINATION_MOUNT}"
MOUNT_STATE=normal

# Pause the controller after a verified copy, remount the same destination
# filesystem read-only, and prove promotion fails before owner commit. Once the
# mount is writable again, source recovery quarantines the verified staging.
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
controller_up
wait_for_blocked PromotionFailed
docker exec "${DESTINATION_NODE}" test -f "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}/.shiftpv-move-id"
docker exec "${DESTINATION_NODE}" test ! -e "${DESTINATION_MOUNT}/volumes/${VOLUME_ID}"
docker exec "${DESTINATION_NODE}" mount -o remount,rw "${DESTINATION_MOUNT}"
MOUNT_STATE=tmpfs_rw
request_source_recovery "${READONLY_NAMESPACE}"
docker exec "${DESTINATION_NODE}" test -f "${DESTINATION_MOUNT}/.shiftpv/aborted/${MOVE_NAME}-incoming/.shiftpv-move-id"
docker exec "${DESTINATION_NODE}" test ! -e "${DESTINATION_MOUNT}/.shiftpv/incoming/${MOVE_NAME}"
echo "mobility read-only promotion recovery passed: volume=${VOLUME_ID} move=${MOVE_NAME}"
delete_workload "${READONLY_NAMESPACE}"
docker exec "${DESTINATION_NODE}" umount "${DESTINATION_MOUNT}"
MOUNT_STATE=normal

echo 'ShiftPV mobility filesystem fault recovery passed'
