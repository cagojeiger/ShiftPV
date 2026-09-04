#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${WORK_DIR:?WORK_DIR is required}"
: "${WORKER_A_POOL:?WORKER_A_POOL is required}"
: "${WORKER_B_POOL:?WORKER_B_POOL is required}"

SOURCE_NODE="${CLUSTER_NAME}-worker"
DESTINATION_NODE="${CLUSTER_NAME}-worker2"
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

pause_before_copy() {
	local deadline=$((SECONDS + 300)) phase="" replacement="" replacement_node=""
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
			replacement=$(kubectl -n "${TEST_NAMESPACE}" get pod -l "app=${TEST_NAMESPACE}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
		fi
		if [[ -n "${replacement}" ]]; then
			replacement_node=$(kubectl -n "${TEST_NAMESPACE}" get "pod/${replacement}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
			[[ "${replacement_node}" == "${DESTINATION_NODE}" ]] && return
		fi
		sleep 0.2
	done
	echo "replacement Pod was not scheduled on ${DESTINATION_NODE}: pod=${replacement} node=${replacement_node}" >&2
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
	test -f "${WORKER_A_POOL}/volumes/${VOLUME_ID}/payload"
	test ! -e "${WORKER_B_POOL}/volumes/${VOLUME_ID}"
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
	kubectl delete "namespace/${namespace}" --wait=true --timeout=180s
	kubectl uncordon "${SOURCE_NODE}" >/dev/null 2>&1 || true
	kubectl uncordon "${DESTINATION_NODE}" >/dev/null 2>&1 || true
}

# Keep each phase visible long enough to pause reconciliation before disk-side
# work. The node failure and recovery are real Kind container stop/start events.
helm upgrade shiftpv "${ROOT_DIR}/charts/shiftpv" \
	--namespace shiftpv-system \
	--reuse-values \
	--set mobility.interval=10s \
	--wait \
	--timeout 5m

run_case shiftpv-node-restart-source 'ShiftPV source node restart recovery' "${SOURCE_NODE}" SourceUnavailable
run_case shiftpv-node-restart-destination 'ShiftPV destination node restart recovery' "${DESTINATION_NODE}" InvalidDestination

echo 'ShiftPV mobility node-container restart recovery passed'
