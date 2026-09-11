#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
# shellcheck source=test/e2e/kind/node-path.sh
source "${ROOT_DIR}/test/e2e/kind/node-path.sh"
: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${WORK_DIR:?WORK_DIR is required}"
: "${WORKER_A_POOL:?WORKER_A_POOL is required}"
: "${WORKER_B_POOL:?WORKER_B_POOL is required}"

SOURCE_NODE="${CLUSTER_NAME}-worker"
DESTINATION_NODE="${CLUSTER_NAME}-worker2"
SOURCE_MOUNT=/mnt/shiftpv
DESTINATION_MOUNT=/srv/shiftpv-b
restore_cluster() {
	for node in "${SOURCE_NODE}" "${DESTINATION_NODE}"; do
		docker start "${node}" >/dev/null 2>&1 || true
	done
	kubectl uncordon "${SOURCE_NODE}" >/dev/null 2>&1 || true
	kubectl uncordon "${DESTINATION_NODE}" >/dev/null 2>&1 || true
	kubectl -n shiftpv-system scale deployment/shiftpv-controller --replicas=1 >/dev/null 2>&1 || true
}
trap restore_cluster EXIT

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

wait_for_node_condition() {
	local node=$1 expected=$2 deadline=$((SECONDS + 180)) condition=""
	while ((SECONDS < deadline)); do
		condition=$(kubectl get "node/${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
		case "${expected}:${condition}" in
		ready:True | unavailable:False | unavailable:Unknown)
			return
			;;
		esac
		sleep 1
	done
	echo "node ${node} did not become ${expected}; Ready=${condition}" >&2
	kubectl get "node/${node}" -o wide >&2 || true
	return 1
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

placement_pod_for_move() {
	local pod owner
	while IFS= read -r pod; do
		[[ -n "${pod}" ]] || continue
		owner=$(kubectl -n shiftpv-system get "${pod}" \
			-o jsonpath='{.metadata.ownerReferences[?(@.kind=="ShiftPVMove")].name}' 2>/dev/null || true)
		if [[ "${owner}" == "${MOVE_NAME}" ]]; then
			printf '%s\n' "${pod#pod/}"
			return
		fi
	done < <(kubectl -n shiftpv-system get pod -l shiftpv.io/role=placement -o name 2>/dev/null || true)
	return 1
}

pause_before_copy() {
	local deadline=$((SECONDS + 300)) phase="" replacement="" replacement_node="" replacement_hold="" placement="" placement_node=""
	while ((SECONDS < deadline)); do
		phase=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		if [[ "${phase}" == WaitingForDestination ]]; then
			controller_down
			break
		fi
		if [[ "${phase}" == Blocked || "${phase}" == Succeeded ]]; then
			echo "Move reached terminal phase before node fault injection: ${phase}" >&2
			return 1
		fi
		sleep 0.2
	done
	if [[ "${phase}" != WaitingForDestination ]]; then
		echo "Move did not reach WaitingForDestination before deadline; phase=${phase}" >&2
		return 1
	fi

	deadline=$((SECONDS + 120))
	while ((SECONDS < deadline)); do
		replacement=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.replacementName}' 2>/dev/null || true)
		if [[ -z "${replacement}" ]]; then
			replacement=$(kubectl -n "${TEST_NAMESPACE}" get pod \
				-l "app=${TEST_NAMESPACE},shiftpv.io/managed=true" \
				-o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
		fi
		if [[ -n "${replacement}" ]]; then
			replacement_node=$(kubectl -n "${TEST_NAMESPACE}" get "pod/${replacement}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
			replacement_hold=$(kubectl -n "${TEST_NAMESPACE}" get "pod/${replacement}" \
				-o jsonpath='{.spec.schedulingGates[?(@.name=="shiftpv.io/placement-hold")].name}' 2>/dev/null || true)
			placement=$(placement_pod_for_move || true)
			if [[ -n "${placement}" ]]; then
				placement_node=$(kubectl -n shiftpv-system get "pod/${placement}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
				if [[ -z "${replacement_node}" && "${replacement_hold}" == "shiftpv.io/placement-hold" && "${placement_node}" == "${DESTINATION_NODE}" ]]; then
					return
				fi
			fi
		fi
		sleep 0.2
	done
	echo "placement reservation was not safely selected on ${DESTINATION_NODE}: replacement=${replacement} replacementNode=${replacement_node} hold=${replacement_hold} placement=${placement} placementNode=${placement_node}" >&2
	return 1
}

pause_at_phase() {
	local expected_phase=$1 deadline=$((SECONDS + 300)) phase=""
	while ((SECONDS < deadline)); do
		phase=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		if [[ "${phase}" == "${expected_phase}" ]]; then
			controller_down
			phase=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}')
			test "${phase}" = "${expected_phase}"
			return
		fi
		if [[ "${phase}" == Blocked || "${phase}" == Succeeded ]]; then
			echo "Move reached terminal phase before ${expected_phase} node fault injection: ${phase}" >&2
			return 1
		fi
		sleep 0.2
	done
	echo "Move did not reach ${expected_phase} before deadline; phase=${phase}" >&2
	return 1
}

stop_node() {
	local node=$1
	docker stop -t 1 "${node}" >/dev/null
	wait_for_node_condition "${node}" unavailable
}

start_node() {
	local node=$1
	docker start "${node}" >/dev/null
	wait_for_node_condition "${node}" ready
	kubectl -n shiftpv-system rollout status daemonset/shiftpv-node --timeout=180s
}

assert_blocked_source_authority() {
	local reason=$1
	kubectl wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.phase}'=Blocked --timeout=180s
	test "$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.reason}')" = "${reason}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = Blocked
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = "${MOVE_NAME}"
	assert_node_file "${SOURCE_NODE}" "${SOURCE_MOUNT}/volumes/${VOLUME_ID}/payload"
	assert_node_absent "${DESTINATION_NODE}" "${DESTINATION_MOUNT}/volumes/${VOLUME_ID}"
}

recover_source() {
	local namespace=$1 pod checksum
	kubectl uncordon "${SOURCE_NODE}"
	kubectl patch "shiftpvmove/${MOVE_NAME}" --type merge -p '{"spec":{"recovery":"ResumeOwner"}}'
	kubectl wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.recoveryPhase}'=Recovered --timeout=300s
	kubectl -n "${namespace}" rollout status deployment/writer --timeout=180s
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

assert_destination_unavailable_wait() {
	local expected_phase=$1 expected_owner=$2
	kubectl wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.reason}'=DestinationUnavailable --timeout=180s
	test "$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}')" = "${expected_phase}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${expected_owner}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = "${MOVE_NAME}"
	assert_node_file "${SOURCE_NODE}" "${SOURCE_MOUNT}/volumes/${VOLUME_ID}/payload"
	if [[ "${expected_owner}" == "${SOURCE_NODE}" ]]; then
		test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = Moving
	else
		test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = Ready
	fi
}

assert_destination_publish_metadata() {
	local namespace=$1 pod=$2 pod_uid volume_data_path placement
	pod_uid=$(kubectl -n "${namespace}" get "pod/${pod}" -o jsonpath='{.metadata.uid}')
	volume_data_path="/var/lib/kubelet/pods/${pod_uid}/volumes/kubernetes.io~csi/${PV_NAME}/vol_data.json"
	if ! docker exec "${DESTINATION_NODE}" test -f "${volume_data_path}"; then
		echo "destination kubelet CSI metadata is missing: ${volume_data_path}" >&2
		docker exec "${DESTINATION_NODE}" find "/var/lib/kubelet/pods/${pod_uid}/volumes" -maxdepth 4 -print >&2 2>/dev/null || true
		return 1
	fi
	if docker exec "${DESTINATION_NODE}" journalctl -u kubelet --no-pager 2>&1 \
		| grep -E 'failed to open volume data file.*vol_data\.json|vol_data\.json.*no such file or directory'; then
		echo 'destination kubelet reported incomplete CSI volume metadata' >&2
		return 1
	fi
	placement=$(placement_pod_for_move || true)
	if [[ -n "${placement}" ]]; then
		echo "placement reservation remained after successful move: ${placement}" >&2
		return 1
	fi
}

finish_destination_move() {
	local namespace=$1 pod checksum
	kubectl wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.phase}'=Succeeded --timeout=600s
	kubectl -n "${namespace}" rollout status deployment/writer --timeout=180s
	pod=$(kubectl -n "${namespace}" get pod -l "app=${namespace}" -o jsonpath='{.items[0].metadata.name}')
	test "$(kubectl -n "${namespace}" get "pod/${pod}" -o jsonpath='{.spec.nodeName}')" = "${DESTINATION_NODE}"
	test "$(kubectl -n "${namespace}" get "pod/${pod}" -o jsonpath='{.metadata.annotations.shiftpv\.io/placement}')" = owner
	test -z "$(kubectl -n "${namespace}" get "pod/${pod}" -o jsonpath='{.spec.schedulingGates[?(@.name=="shiftpv.io/placement-hold")].name}')"
	checksum=$(kubectl -n "${namespace}" exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')
	test "${checksum}" = "${SOURCE_CHECKSUM}"
	test "$(kubectl -n "${namespace}" get pvc/data -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
	test "$(kubectl -n "${namespace}" get pvc/data -o jsonpath='{.spec.volumeName}')" = "${PV_NAME}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = Ready
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${DESTINATION_NODE}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = ""
	assert_node_absent "${SOURCE_NODE}" "${SOURCE_MOUNT}/volumes/${VOLUME_ID}"
	assert_node_file "${DESTINATION_NODE}" "${DESTINATION_MOUNT}/volumes/${VOLUME_ID}/payload"
	assert_destination_publish_metadata "${namespace}" "${pod}"
}

cleanup_case() {
	local namespace=$1
	kubectl delete "namespace/${namespace}" --wait=true --timeout=180s
	kubectl uncordon "${SOURCE_NODE}" >/dev/null 2>&1 || true
	kubectl uncordon "${DESTINATION_NODE}" >/dev/null 2>&1 || true
}

run_case() {
	local namespace=$1 payload=$2 stopped_node=$3 expected_reason=$4
	TEST_NAMESPACE="${namespace}"
	create_source_workload "${namespace}" "${payload}"
	kubectl cordon "${SOURCE_NODE}"
	wait_for_move
	pause_before_copy
	stop_node "${stopped_node}"
	# When the destination is down, the source is the only worker that can host
	# the restarted controller. The transaction is already past preflight, so
	# uncordoning it does not authorize a new move or change volume authority.
	if [[ "${stopped_node}" == "${DESTINATION_NODE}" ]]; then
		kubectl uncordon "${SOURCE_NODE}"
	fi
	controller_up
	assert_blocked_source_authority "${expected_reason}"
	start_node "${stopped_node}"
	recover_source "${namespace}"
	echo "mobility node restart recovery passed: node=${stopped_node} reason=${expected_reason} volume=${VOLUME_ID} move=${MOVE_NAME} checksum=${SOURCE_CHECKSUM}"
	cleanup_case "${namespace}"
}

run_copying_source_restart_case() {
	local namespace=$1 payload=$2
	TEST_NAMESPACE="${namespace}"
	create_source_workload "${namespace}" "${payload}"
	kubectl cordon "${SOURCE_NODE}"
	wait_for_move
	pause_at_phase Copying
	stop_node "${SOURCE_NODE}"
	controller_up
	assert_blocked_source_authority SourceUnavailable
	start_node "${SOURCE_NODE}"
	recover_source "${namespace}"
	echo "mobility Copying source restart recovery passed: volume=${VOLUME_ID} move=${MOVE_NAME} checksum=${SOURCE_CHECKSUM}"
	cleanup_case "${namespace}"
}

run_pre_copy_destination_restart_case() {
	local namespace=$1 payload=$2
	TEST_NAMESPACE="${namespace}"
	create_source_workload "${namespace}" "${payload}"
	kubectl cordon "${SOURCE_NODE}"
	wait_for_move
	pause_before_copy
	stop_node "${DESTINATION_NODE}"
	# The destination reservation is durable, but the source remains authoritative.
	# Let the controller run on the source and wait without releasing the workload.
	kubectl uncordon "${SOURCE_NODE}"
	controller_up
	assert_destination_unavailable_wait WaitingForCapacity "${SOURCE_NODE}"
	start_node "${DESTINATION_NODE}"
	finish_destination_move "${namespace}"
	echo "mobility pre-copy destination restart continuation passed: volume=${VOLUME_ID} move=${MOVE_NAME} checksum=${SOURCE_CHECKSUM}"
	cleanup_case "${namespace}"
}

run_destination_restart_case() {
	local namespace=$1 payload=$2 fault_phase=$3 expected_owner=$4
	TEST_NAMESPACE="${namespace}"
	create_source_workload "${namespace}" "${payload}"
	kubectl cordon "${SOURCE_NODE}"
	wait_for_move
	pause_at_phase "${fault_phase}"
	if [[ "${expected_owner}" == "${DESTINATION_NODE}" ]]; then
		test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${DESTINATION_NODE}"
	else
		test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
	fi
	stop_node "${DESTINATION_NODE}"
	# The transaction is already past preflight. Uncordoning the source lets the
	# controller run while the selected destination is unavailable; it does not
	# change the persisted destination or volume authority.
	kubectl uncordon "${SOURCE_NODE}"
	controller_up
	assert_destination_unavailable_wait "${fault_phase}" "${expected_owner}"
	start_node "${DESTINATION_NODE}"
	finish_destination_move "${namespace}"
	echo "mobility ${fault_phase} destination restart continuation passed: volume=${VOLUME_ID} move=${MOVE_NAME} checksum=${SOURCE_CHECKSUM}"
	cleanup_case "${namespace}"
}

# Keep each phase visible long enough to pause reconciliation before disk-side
# work. The node failure and recovery are real Kind container stop/start events.
helm upgrade shiftpv "${ROOT_DIR}/charts/shiftpv" \
	--namespace shiftpv-system \
	--reuse-values \
	--set mobility.interval=10s \
	--wait \
	--timeout 5m

case ${MOBILITY_NODE_RESTART_CASE:-all} in
all)
	run_case shiftpv-node-restart-source 'ShiftPV source node restart recovery' "${SOURCE_NODE}" SourceUnavailable
	run_pre_copy_destination_restart_case shiftpv-node-restart-destination 'ShiftPV pre-copy destination node restart continuation'
	run_copying_source_restart_case shiftpv-node-restart-copying-source 'ShiftPV Copying source restart recovery'
	run_destination_restart_case shiftpv-node-restart-copying-destination 'ShiftPV Copying destination restart continuation' Copying "${SOURCE_NODE}"
	run_destination_restart_case shiftpv-node-restart-promoting-destination 'ShiftPV Promoting destination restart continuation' Promoting "${SOURCE_NODE}"
	run_destination_restart_case shiftpv-node-restart-committed-destination 'ShiftPV committed destination restart continuation' WaitingForDestinationPublish "${DESTINATION_NODE}"
	;;
source)
	run_case shiftpv-node-restart-source 'ShiftPV source node restart recovery' "${SOURCE_NODE}" SourceUnavailable
	;;
destination)
	run_pre_copy_destination_restart_case shiftpv-node-restart-destination 'ShiftPV pre-copy destination node restart continuation'
	;;
copying-source)
	run_copying_source_restart_case shiftpv-node-restart-copying-source 'ShiftPV Copying source restart recovery'
	;;
copying-destination)
	run_destination_restart_case shiftpv-node-restart-copying-destination 'ShiftPV Copying destination restart continuation' Copying "${SOURCE_NODE}"
	;;
promoting-destination)
	run_destination_restart_case shiftpv-node-restart-promoting-destination 'ShiftPV Promoting destination restart continuation' Promoting "${SOURCE_NODE}"
	;;
committed-destination)
	run_destination_restart_case shiftpv-node-restart-committed-destination 'ShiftPV committed destination restart continuation' WaitingForDestinationPublish "${DESTINATION_NODE}"
	;;
*)
	echo "unsupported MOBILITY_NODE_RESTART_CASE: ${MOBILITY_NODE_RESTART_CASE}" >&2
	exit 1
	;;
esac

echo 'ShiftPV mobility node-container restart recovery passed'
