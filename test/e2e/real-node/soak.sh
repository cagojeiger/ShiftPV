#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
# shellcheck source=test/e2e/real-node/lib.sh
source "${ROOT_DIR}/test/e2e/real-node/lib.sh"

require_real_node_env

apply_real_node_defaults
TEST_NAMESPACE=${TEST_NAMESPACE:-shiftpv-real-node-soak}
ITERATIONS=${ITERATIONS:-100}
MIN_DURATION_SECONDS=${MIN_DURATION_SECONDS:-43200}
CONTROLLER_RESTART_EVERY=${CONTROLLER_RESTART_EVERY:-10}
CLEANUP_POD_DELETE_EVERY=${CLEANUP_POD_DELETE_EVERY:-20}
MOVE_TIMEOUT_SECONDS=${MOVE_TIMEOUT_SECONDS:-900}
ARTIFACT_DIR=${ARTIFACT_DIR:-${ROOT_DIR}/.tmp/real-node/soak-${RUN_ID}}

if ! [[ "${ITERATIONS}" =~ ^[1-9][0-9]*$ && "${MIN_DURATION_SECONDS}" =~ ^[0-9]+$ &&
	"${CONTROLLER_RESTART_EVERY}" =~ ^[0-9]+$ && "${CLEANUP_POD_DELETE_EVERY}" =~ ^[0-9]+$ ]]; then
	echo 'iteration, duration, and fault interval values must be non-negative integers; ITERATIONS must be positive' >&2
	exit 1
fi
if [[ "${FAULT_NODE}" != "${SOURCE_NODE}" || "${FAULT_SSH_TARGET}" != "${SOURCE_SSH_TARGET}" ]]; then
	echo 'soak preflight requires the reviewed fault identity to match the initial source node' >&2
	exit 1
fi

require_real_node_commands

mkdir -p "${ARTIFACT_DIR}"

trap report_error ERR

restore_environment() {
	local result_code=$? restore_failed=0
	trap - ERR EXIT INT TERM
	set +e
	if ! k uncordon "${SOURCE_NODE}" >/dev/null 2>&1; then
		restore_failed=1
		warn_uncordon_needed "${SOURCE_NODE}" "uncordon failed for ${SOURCE_NODE}"
	fi
	if ! k uncordon "${DESTINATION_NODE}" >/dev/null 2>&1; then
		restore_failed=1
		warn_uncordon_needed "${DESTINATION_NODE}" "uncordon failed for ${DESTINATION_NODE}"
	fi
	capture_evidence final
	if ((result_code != 0)); then
		echo "soak failed; test resources were preserved for diagnosis in ${ARTIFACT_DIR}" >&2
	fi
	if ((result_code == 0 && restore_failed)); then
		result_code=1
	fi
	exit "${result_code}"
}
trap restore_environment EXIT INT TERM

wait_for_move() {
	local previous_move=$1 deadline=$((SECONDS + MOVE_TIMEOUT_SECONDS)) move='' after=''
	if [[ -n "${previous_move}" ]]; then
		after=$(k get "shiftpvmove/${previous_move}" -o jsonpath='{.metadata.creationTimestamp}')
		test -n "${after}"
	fi
	while ((SECONDS < deadline)); do
		move=$(k get shiftpvmoves -o json | jq -r --arg volume "${VOLUME_ID}" --arg after "${after}" '
			[.items[] | select(.spec.volumeID == $volume and (.metadata.creationTimestamp > $after))]
			| sort_by(.metadata.creationTimestamp) | last | .metadata.name // ""')
		if [[ -n "${move}" ]]; then
			printf '%s\n' "${move}"
			return
		fi
		sleep 1
	done
	echo "new move was not created for ${VOLUME_ID}" >&2
	return 1
}

wait_for_move_settled() {
	local move=$1 deadline=$((SECONDS + MOVE_TIMEOUT_SECONDS)) phase='' reason='' cleanup_phase='' finalizer_count=''
	while ((SECONDS < deadline)); do
		read -r phase cleanup_phase finalizer_count < <(k get "shiftpvmove/${move}" -o json | jq -r '
			[.status.phase // "", .status.cleanup.status.phase // "", ((.metadata.finalizers // []) | length)] | @tsv' 2>/dev/null || true)
		case "${phase}" in
		Succeeded)
			if [[ "${cleanup_phase}" == Completed && "${finalizer_count}" == 0 ]]; then
				return
			fi
			;;
		Blocked)
			reason=$(k get "shiftpvmove/${move}" -o jsonpath='{.status.reason}' 2>/dev/null || true)
			echo "move blocked during soak: move=${move} reason=${reason}" >&2
			return 1
			;;
		esac
		sleep 1
	done
	echo "move did not settle: move=${move} phase=${phase} cleanup=${cleanup_phase} finalizers=${finalizer_count}" >&2
	return 1
}

wait_for_pools_current() {
	local deadline=$((SECONDS + 300)) stale=''
	while ((SECONDS < deadline)); do
		stale=$(k get shiftpvpools -o json | jq '
			[.items[]
			 | select(
			     .status.inventory.valid != true or
			     (.status.inventory.truncated // false) == true or
			     .metadata.generation != .status.observedGeneration)]
			| length')
		if [[ "${stale}" == 0 ]]; then
			return
		fi
		sleep 1
	done
	echo "Pool inventory did not become current: stale=${stale}" >&2
	return 1
}

restart_controller() {
	local pod
	pod=$(k -n "${SYSTEM_NAMESPACE}" get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}')
	k -n "${SYSTEM_NAMESPACE}" delete "pod/${pod}" --wait=true --timeout=180s
	k -n "${SYSTEM_NAMESPACE}" rollout status deployment/shiftpv-controller --timeout=300s
}

delete_cleanup_pod() {
	local move=$1 deadline=$((SECONDS + MOVE_TIMEOUT_SECONDS)) phase='' cleanup_phase='' job='' pod=''
	while ((SECONDS < deadline)); do
		phase=$(k get "shiftpvmove/${move}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		cleanup_phase=$(k get "shiftpvmove/${move}" -o jsonpath='{.status.cleanup.status.phase}' 2>/dev/null || true)
		job=$(k get "shiftpvmove/${move}" -o jsonpath='{.status.cleanup.status.executor.jobName}' 2>/dev/null || true)
		if [[ "${cleanup_phase}" == Running && -n "${job}" ]]; then
			pod=$(k -n "${SYSTEM_NAMESPACE}" get pod -l "job-name=${job}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
			if [[ -n "${pod}" ]]; then
				k -n "${SYSTEM_NAMESPACE}" delete "pod/${pod}" --wait=true --timeout=180s
				return
			fi
		fi
		if [[ "${phase}" == Succeeded || "${phase}" == Blocked || "${cleanup_phase}" == Completed || "${cleanup_phase}" == NeedsReview ]]; then
			echo "missed cleanup Pod deletion boundary: move=${move} phase=${phase} cleanup=${cleanup_phase}" >&2
			return 1
		fi
		sleep 0.1
	done
	echo "cleanup Pod did not become injectable: move=${move}" >&2
	return 1
}

assert_iteration() {
	local iteration=$1 move=$2 expected_node=$3 old_node=$4 pod checksum cleanup_phase finalizers old_pool expected_pool old_role expected_role
	k -n "${TEST_NAMESPACE}" rollout status deployment/writer --timeout=600s
	pod=$(k -n "${TEST_NAMESPACE}" get pod -l app=shiftpv-real-node-soak -o json | jq -r --arg node "${expected_node}" '
		[.items[]
		 | select(.metadata.deletionTimestamp == null and .spec.nodeName == $node and .status.phase == "Running")
		 | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))]
		| if length == 1 then .[0].metadata.name else "" end')
	test -n "${pod}"
	checksum=$(k -n "${TEST_NAMESPACE}" exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')
	test "${checksum}" = "${SOURCE_CHECKSUM}"
	test "$(k -n "${TEST_NAMESPACE}" get pvc/data -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
	test "$(k -n "${TEST_NAMESPACE}" get pvc/data -o jsonpath='{.spec.volumeName}')" = "${PV_NAME}"
	test "$(k get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')" = "${VOLUME_ID}"
	test "$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = Ready
	test "$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${expected_node}"
	cleanup_phase=$(k get "shiftpvmove/${move}" -o jsonpath='{.status.cleanup.status.phase}')
	finalizers=$(k get "shiftpvmove/${move}" -o jsonpath='{.metadata.finalizers}')
	test "${cleanup_phase}" = Completed
	test -z "${finalizers}"
	k get "shiftpvmove/${move}" -o json | jq -e '
		.status.cleanup.status.phase == "Completed" and
		.status.cleanup.status.receipt.purged == true and
		.status.cleanup.status.receipt.retired == true and
		.status.cleanup.spec.operationID == .status.cleanup.status.receipt.operationID and
		.status.cleanup.status.executor.jobUID == .status.cleanup.status.receipt.executorUID and
		(.status.cleanup.status.receipt.localReceiptDigest | test("^[0-9a-f]{64}$"))' >/dev/null
	if [[ "${old_node}" == "${SOURCE_NODE}" ]]; then
		old_role=source
		expected_role=destination
		old_pool=${SOURCE_POOL}
		expected_pool=${DESTINATION_POOL}
	else
		old_role=destination
		expected_role=source
		old_pool=${DESTINATION_POOL}
		expected_pool=${SOURCE_POOL}
	fi
	ssh_node "${old_role}" sudo test ! -e "${old_pool}/volumes/${VOLUME_ID}"
	ssh_node "${expected_role}" sudo test -f "${expected_pool}/volumes/${VOLUME_ID}/payload"
	k wait shiftpvpool --all --for=condition=Ready --timeout=300s >/dev/null
	wait_for_pools_current
	printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
		"${iteration}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${move}" "${old_node}" "${expected_node}" "${checksum}" |
		tee -a "${ARTIFACT_DIR}/iterations.tsv"
}

cleanup_test_volume() {
	local pv_name=$1 volume_id=$2
	k patch "pv/${pv_name}" --type=merge -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
	k delete "namespace/${TEST_NAMESPACE}" --wait=true --timeout=300s
	k wait "pv/${pv_name}" --for=delete --timeout=300s
	k wait "shiftpvvolume/${volume_id}" --for=delete --timeout=300s
}

"${ROOT_DIR}/test/e2e/real-node/preflight.sh" | tee "${ARTIFACT_DIR}/preflight.txt"
if k get "namespace/${TEST_NAMESPACE}" >/dev/null 2>&1; then
	echo "test namespace already exists: ${TEST_NAMESPACE}" >&2
	exit 1
fi
snapshot_non_shiftpv_specs before exact "${TEST_NAMESPACE}"
capture_evidence before

k cordon "${DESTINATION_NODE}" >/dev/null
create_test_workload "${TEST_NAMESPACE}" shiftpv-real-node-soak 128Mi 64 'ShiftPV real-node alternating soak'
k -n "${TEST_NAMESPACE}" rollout status deployment/writer --timeout=600s
k -n "${TEST_NAMESPACE}" wait pvc/data --for=jsonpath='{.status.phase}'=Bound --timeout=180s
pod=$(k -n "${TEST_NAMESPACE}" get pod -l app=shiftpv-real-node-soak -o jsonpath='{.items[0].metadata.name}')
test "$(k -n "${TEST_NAMESPACE}" get "pod/${pod}" -o jsonpath='{.spec.nodeName}')" = "${SOURCE_NODE}"
PVC_UID=$(k -n "${TEST_NAMESPACE}" get pvc/data -o jsonpath='{.metadata.uid}')
PV_NAME=$(k -n "${TEST_NAMESPACE}" get pvc/data -o jsonpath='{.spec.volumeName}')
VOLUME_ID=$(k get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
SOURCE_CHECKSUM=$(k -n "${TEST_NAMESPACE}" exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')
SOURCE_POOL=$(k get shiftpvpools -o jsonpath="{.items[?(@.spec.nodeName=='${SOURCE_NODE}')].spec.mountPath}")
DESTINATION_POOL=$(k get shiftpvpools -o jsonpath="{.items[?(@.spec.nodeName=='${DESTINATION_NODE}')].spec.mountPath}")
test -n "${SOURCE_POOL}"
test -n "${DESTINATION_POOL}"
k uncordon "${DESTINATION_NODE}" >/dev/null

printf 'iteration\ttimestamp\tmove\tsource\tdestination\tchecksum\n' >"${ARTIFACT_DIR}/iterations.tsv"
start_seconds=${SECONDS}
current_node=${SOURCE_NODE}
last_move=''
for ((iteration = 1; iteration <= ITERATIONS; iteration++)); do
	if [[ "${current_node}" == "${SOURCE_NODE}" ]]; then
		next_node=${DESTINATION_NODE}
	else
		next_node=${SOURCE_NODE}
	fi
	k uncordon "${next_node}" >/dev/null
	k cordon "${current_node}" >/dev/null
	move=$(wait_for_move "${last_move}")
	if ((CONTROLLER_RESTART_EVERY > 0 && iteration % CONTROLLER_RESTART_EVERY == 0)); then
		restart_controller
	fi
	if ((CLEANUP_POD_DELETE_EVERY > 0 && iteration % CLEANUP_POD_DELETE_EVERY == 0)); then
		delete_cleanup_pod "${move}"
	fi
	wait_for_move_settled "${move}"
	assert_iteration "${iteration}" "${move}" "${next_node}" "${current_node}"
	k uncordon "${current_node}" >/dev/null
	current_node=${next_node}
	last_move=${move}
	target_seconds=$((start_seconds + (iteration * MIN_DURATION_SECONDS / ITERATIONS)))
	while ((SECONDS < target_seconds)); do
		sleep 5
	done
done

elapsed_seconds=$((SECONDS - start_seconds))
cleanup_test_volume "${PV_NAME}" "${VOLUME_ID}"
k wait shiftpvpool --all --for=condition=Ready --timeout=300s
test "$(k get shiftpvvolumes -o json | jq '.items | length')" = 0
assert_no_unsettled_moves
snapshot_non_shiftpv_specs after exact "${TEST_NAMESPACE}"
for subject in non-shiftpv-workloads existing-pvcs existing-pvs storageclasses; do
	diff -u "${ARTIFACT_DIR}/before-${subject}.json" "${ARTIFACT_DIR}/after-${subject}.json" >"${ARTIFACT_DIR}/${subject}.diff"
done
capture_evidence passed

if ((ITERATIONS >= 100 && elapsed_seconds >= 43200)); then
	result_marker=REAL_NODE_SOAK_OK
else
	result_marker=REAL_NODE_SOAK_SMOKE_OK
fi
printf '%s context=%s iterations=%d elapsedSeconds=%d artifacts=%s\n' \
	"${result_marker}" "${KUBECTL_CONTEXT}" "${ITERATIONS}" "${elapsed_seconds}" "${ARTIFACT_DIR}"
