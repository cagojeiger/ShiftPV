#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)

: "${KUBECTL_CONTEXT:?KUBECTL_CONTEXT is required}"
: "${SOURCE_NODE:?SOURCE_NODE is required}"
: "${DESTINATION_NODE:?DESTINATION_NODE is required}"
: "${FAULT_NODE:?FAULT_NODE is required}"
: "${FAULT_SSH_TARGET:?FAULT_SSH_TARGET is required}"
: "${SOURCE_SSH_TARGET:?SOURCE_SSH_TARGET is required}"
: "${DESTINATION_SSH_TARGET:?DESTINATION_SSH_TARGET is required}"
: "${EXPECTED_CONTROLLER_IMAGE:?EXPECTED_CONTROLLER_IMAGE is required}"
: "${EXPECTED_NODE_IMAGE:?EXPECTED_NODE_IMAGE is required}"
EXPECTED_NON_DAEMONSET_PODS_SHA256=${EXPECTED_NON_DAEMONSET_PODS_SHA256:-}
export EXPECTED_NON_DAEMONSET_PODS_SHA256

SYSTEM_NAMESPACE=${SYSTEM_NAMESPACE:-shiftpv-system}
STORAGE_CLASS=${STORAGE_CLASS:-shiftpv}
TEST_NAMESPACE=${TEST_NAMESPACE:-shiftpv-real-node-soak}
ITERATIONS=${ITERATIONS:-100}
MIN_DURATION_SECONDS=${MIN_DURATION_SECONDS:-43200}
CONTROLLER_RESTART_EVERY=${CONTROLLER_RESTART_EVERY:-10}
CLEANUP_POD_DELETE_EVERY=${CLEANUP_POD_DELETE_EVERY:-20}
MOVE_TIMEOUT_SECONDS=${MOVE_TIMEOUT_SECONDS:-900}
SSH_CONNECT_TIMEOUT=${SSH_CONNECT_TIMEOUT:-5}
WORKLOAD_IMAGE=${WORKLOAD_IMAGE:-busybox:1.37@sha256:7a3ebe5bfd1a4a19797d20b0c0bb39d44393e9a03fd852c0865b0f540d868df0}
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
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

for command in kubectl jq ssh diff tee; do
	command -v "${command}" >/dev/null || {
		echo "required command not found: ${command}" >&2
		exit 1
	}
done

mkdir -p "${ARTIFACT_DIR}"

report_error() {
	local result_code=$?
	trap - ERR
	printf 'FAIL line=%s exit=%d command=%s\n' "${BASH_LINENO[0]}" "${result_code}" "${BASH_COMMAND}" |
		tee -a "${ARTIFACT_DIR}/failure.txt" >&2
	exit "${result_code}"
}
trap report_error ERR

k() {
	kubectl --context "${KUBECTL_CONTEXT}" --request-timeout=30s "$@"
}

ssh_node() {
	local node=$1
	shift
	case "${node}" in
	"${SOURCE_NODE}") ssh -o BatchMode=yes -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}" "${SOURCE_SSH_TARGET}" "$@" ;;
	"${DESTINATION_NODE}") ssh -o BatchMode=yes -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}" "${DESTINATION_SSH_TARGET}" "$@" ;;
	*) echo "unknown soak node: ${node}" >&2; return 1 ;;
	esac
}

capture_evidence() {
	local label=$1
	k get nodes -o wide >"${ARTIFACT_DIR}/${label}-nodes.txt" 2>&1 || true
	k get pods -A -o wide >"${ARTIFACT_DIR}/${label}-pods.txt" 2>&1 || true
	k get pvc,pv -A -o wide >"${ARTIFACT_DIR}/${label}-storage.txt" 2>&1 || true
	k get shiftpvpools,shiftpvvolumes,shiftpvmoves -o yaml >"${ARTIFACT_DIR}/${label}-shiftpv.yaml" 2>&1 || true
	k get events -A --sort-by=.metadata.creationTimestamp >"${ARTIFACT_DIR}/${label}-events.txt" 2>&1 || true
	k -n "${SYSTEM_NAMESPACE}" logs deployment/shiftpv-controller --all-containers --tail=-1 >"${ARTIFACT_DIR}/${label}-controller.log" 2>&1 || true
}

snapshot_non_shiftpv_specs() {
	local label=$1 autoscaled_workloads
	autoscaled_workloads=$(k get horizontalpodautoscalers.autoscaling -A -o json | jq -c '
		[.items[] | {namespace: .metadata.namespace, kind: .spec.scaleTargetRef.kind, name: .spec.scaleTargetRef.name}]')
	k get deployments.apps,statefulsets.apps -A -o json | jq -S \
		--arg system "${SYSTEM_NAMESPACE}" --arg test "${TEST_NAMESPACE}" \
		--argjson autoscaled "${autoscaled_workloads}" '
		[.items[]
		 | select(.metadata.namespace != $system and .metadata.namespace != $test)
		 | . as $workload
		 | {apiVersion, kind, metadata: {namespace: .metadata.namespace, name: .metadata.name, uid: .metadata.uid},
		    spec: (if any($autoscaled[];
		      .namespace == $workload.metadata.namespace and
		      .kind == $workload.kind and
		      .name == $workload.metadata.name)
		    then ($workload.spec | del(.replicas))
		    else $workload.spec end)}]
		| sort_by(.apiVersion, .kind, .metadata.namespace, .metadata.name)' >"${ARTIFACT_DIR}/${label}-non-shiftpv-workloads.json"
	k get pvc -A -o json | jq -S --arg test "${TEST_NAMESPACE}" '
		[.items[]
		 | select(.metadata.namespace != $test)
		 | {metadata: {namespace: .metadata.namespace, name: .metadata.name, uid: .metadata.uid}, spec}]
		| sort_by(.metadata.namespace, .metadata.name)' >"${ARTIFACT_DIR}/${label}-existing-pvcs.json"
	k get pv -o json | jq -S --arg test "${TEST_NAMESPACE}" '
		[.items[]
		 | select((.spec.claimRef.namespace // "") != $test)
		 | {metadata: {name: .metadata.name, uid: .metadata.uid}, spec}]
		| sort_by(.metadata.name)' >"${ARTIFACT_DIR}/${label}-existing-pvs.json"
	k get storageclass -o json | jq -S '
		[.items[] | {metadata: {name: .metadata.name, uid: .metadata.uid}, provisioner, reclaimPolicy, volumeBindingMode, allowVolumeExpansion, mountOptions, parameters}]
		| sort_by(.metadata.name)' >"${ARTIFACT_DIR}/${label}-storageclasses.json"
}

warn_uncordon_needed() {
	local node=$1 reason=$2
	{
		printf '\n!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n'
		printf 'WARNING: %s\n' "${reason}"
		printf 'Node %s was left cordoned. Run this once resolved:\n' "${node}"
		printf '  kubectl --context %s uncordon %s\n' "${KUBECTL_CONTEXT}" "${node}"
		printf '!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n\n'
	} | tee -a "${ARTIFACT_DIR}/failure.txt" >&2
}

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
	local iteration=$1 move=$2 expected_node=$3 old_node=$4 pod checksum cleanup_phase finalizers old_pool expected_pool
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
		old_pool=${SOURCE_POOL}
		expected_pool=${DESTINATION_POOL}
	else
		old_pool=${DESTINATION_POOL}
		expected_pool=${SOURCE_POOL}
	fi
	ssh_node "${old_node}" sudo test ! -e "${old_pool}/volumes/${VOLUME_ID}"
	ssh_node "${expected_node}" sudo test -f "${expected_pool}/volumes/${VOLUME_ID}/payload"
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

assert_no_unsettled_moves() {
	local unsettled
	unsettled=$(k get shiftpvmoves -o json | jq -r '
		.items[]
		| select(
			((.status.phase == "Succeeded" and .status.cleanup.status.phase == "Completed") or
			 (.status.phase == "Blocked" and .status.recoveryPhase == "Recovered" and
			  (.status.capacityApproved // false) == false and .status.capacityReason == "RecoverySettled")) and
			((.metadata.finalizers // []) | length) == 0
		  | not)
		| .metadata.name')
	if [[ -n "${unsettled}" ]]; then
		echo "unsettled Move journals remain: ${unsettled}" >&2
		return 1
	fi
}

"${ROOT_DIR}/test/e2e/real-node/preflight.sh" | tee "${ARTIFACT_DIR}/preflight.txt"
if k get "namespace/${TEST_NAMESPACE}" >/dev/null 2>&1; then
	echo "test namespace already exists: ${TEST_NAMESPACE}" >&2
	exit 1
fi
snapshot_non_shiftpv_specs before
capture_evidence before

k cordon "${DESTINATION_NODE}" >/dev/null
k apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${TEST_NAMESPACE}
  labels:
    shiftpv.io/admission: enabled
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: ${TEST_NAMESPACE}
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: 128Mi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: writer
  namespace: ${TEST_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: shiftpv-real-node-soak
  template:
    metadata:
      labels:
        app: shiftpv-real-node-soak
    spec:
      containers:
        - name: writer
          image: ${WORKLOAD_IMAGE}
          command: [sh, -ec]
          args:
            - |
              if [ ! -f /data/payload ]; then
                dd if=/dev/zero of=/data/payload bs=1M count=64
                printf '%s\\n' 'ShiftPV real-node alternating soak' >>/data/payload
                sync
              fi
              sleep 86400
          readinessProbe:
            exec:
              command: [test, -f, /data/payload]
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: data
EOF
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
snapshot_non_shiftpv_specs after
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
