#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
# shellcheck source=test/e2e/real-node/lib.sh
source "${ROOT_DIR}/test/e2e/real-node/lib.sh"

require_real_node_env

apply_real_node_defaults
FAULT_MODE=${FAULT_MODE:-service}
case "${FAULT_MODE}" in
service)
	TEST_PREFIX=${TEST_PREFIX:-shiftpv-real-node-stage2}
	RESULT_MARKER=REAL_NODE_SERVICE_INTERRUPTION_OK
	;;
reboot)
	TEST_PREFIX=${TEST_PREFIX:-shiftpv-real-node-stage3}
	RESULT_MARKER=REAL_NODE_OS_REBOOT_OK
	;;
*)
	echo "unsupported FAULT_MODE: ${FAULT_MODE}; expected service or reboot" >&2
	exit 1
	;;
esac
PHASE_TIMEOUT_SECONDS=${PHASE_TIMEOUT_SECONDS:-300}
NODE_TIMEOUT_SECONDS=${NODE_TIMEOUT_SECONDS:-300}
ARTIFACT_DIR=${ARTIFACT_DIR:-${ROOT_DIR}/.tmp/real-node/${RUN_ID}}

if [[ "${FAULT_NODE}" != "${SOURCE_NODE}" || "${FAULT_SSH_TARGET}" != "${SOURCE_SSH_TARGET}" ]]; then
	echo 'real-node qualification requires the fault node to be the source node and its SSH identity to match' >&2
	exit 1
fi

require_real_node_commands

mkdir -p "${ARTIFACT_DIR}"

trap report_error ERR

ssh_source() {
	ssh -o BatchMode=yes -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}" "${SOURCE_SSH_TARGET}" "$@"
}

ssh_destination() {
	ssh -o BatchMode=yes -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}" "${DESTINATION_SSH_TARGET}" "$@"
}

restore_environment() {
	local result_code=$? deadline source_ready=1 restore_failed=0
	trap - ERR EXIT INT TERM
	set +e
	if [[ "${FAULT_MODE}" == reboot ]]; then
		source_ready=0
		deadline=$((SECONDS + NODE_TIMEOUT_SECONDS))
		while ((SECONDS < deadline)); do
			if ssh_source true >/dev/null 2>&1; then
				source_ready=1
				break
			fi
			sleep 2
		done
	fi
	ssh_source sudo snap start microk8s >/dev/null 2>&1
	if ((source_ready)); then
		if ! k uncordon "${SOURCE_NODE}" >/dev/null 2>&1; then
			restore_failed=1
			warn_uncordon_needed "${SOURCE_NODE}" "uncordon failed for ${SOURCE_NODE}"
		fi
	else
		restore_failed=1
		warn_uncordon_needed "${SOURCE_NODE}" \
			"${SOURCE_NODE} did not become reachable within ${NODE_TIMEOUT_SECONDS}s after reboot; it was deliberately left cordoned"
	fi
	if ! k uncordon "${DESTINATION_NODE}" >/dev/null 2>&1; then
		restore_failed=1
		warn_uncordon_needed "${DESTINATION_NODE}" "uncordon failed for ${DESTINATION_NODE}"
	fi
	capture_evidence final source-kubelite
	if ((result_code != 0)); then
		echo "qualification failed; test resources were preserved for diagnosis in ${ARTIFACT_DIR}" >&2
	fi
	if ((result_code == 0 && restore_failed)); then
		result_code=1
	fi
	exit "${result_code}"
}
trap restore_environment EXIT INT TERM

wait_for_node() {
	local node=$1 expected=$2 deadline=$((SECONDS + NODE_TIMEOUT_SECONDS)) ready=''
	while ((SECONDS < deadline)); do
		ready=$(k get "node/${node}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
		case "${expected}:${ready}" in
		ready:True | unavailable:False | unavailable:Unknown)
			return
			;;
		esac
		sleep 2
	done
	echo "node ${node} did not become ${expected}; Ready=${ready}" >&2
	return 1
}

stop_source_node() {
	local boot_id_before='' boot_id_after='' deadline reboot_ssh_pid=''
	if [[ "${FAULT_MODE}" == reboot ]]; then
		boot_id_before=$(ssh_source cat /proc/sys/kernel/random/boot_id)
		test -n "${boot_id_before}"
		# Double force asks systemd to reboot immediately without an orderly unit
		# shutdown. The host returns automatically, so this remains distinct from
		# the hard-power stage that requires independent out-of-band recovery.
		# Run SSH asynchronously because some clients retain the dead connection
		# even after the host has completed the reboot.
		ssh -o BatchMode=yes -o ConnectTimeout="${SSH_CONNECT_TIMEOUT}" \
			"${SOURCE_SSH_TARGET}" sudo systemctl reboot --force --force \
			</dev/null >/dev/null 2>&1 &
		reboot_ssh_pid=$!
		deadline=$((SECONDS + NODE_TIMEOUT_SECONDS))
		while ((SECONDS < deadline)); do
			boot_id_after=$(ssh_source cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)
			if [[ -n "${boot_id_after}" && "${boot_id_after}" != "${boot_id_before}" ]]; then
				break
			fi
			sleep 2
		done
		if [[ -z "${boot_id_after}" || "${boot_id_after}" == "${boot_id_before}" ]]; then
			kill "${reboot_ssh_pid}" >/dev/null 2>&1 || true
			wait "${reboot_ssh_pid}" 2>/dev/null || true
			echo "source host did not return with a new boot ID" >&2
			return 1
		fi
		kill "${reboot_ssh_pid}" >/dev/null 2>&1 || true
		wait "${reboot_ssh_pid}" 2>/dev/null || true
		printf 'PASS source OS reboot bootID=%s->%s\n' "${boot_id_before}" "${boot_id_after}" |
			tee -a "${ARTIFACT_DIR}/reboots.txt"
	fi
	# Stop the runtime and kubelet together. In reboot mode this deterministic
	# hold starts as soon as SSH returns, keeping the fault window open until the
	# control plane observes the node unavailable.
	ssh_source sudo systemctl stop \
		snap.microk8s.daemon-containerd.service \
		snap.microk8s.daemon-kubelite.service
	wait_for_node "${SOURCE_NODE}" unavailable
}

start_source_node() {
	ssh_source sudo snap start microk8s
	wait_for_node "${SOURCE_NODE}" ready
	k -n "${SYSTEM_NAMESPACE}" rollout status daemonset/shiftpv-node --timeout=300s
}

ensure_controller_on_destination() {
	local pod node
	k cordon "${SOURCE_NODE}" >/dev/null
	pod=$(k -n "${SYSTEM_NAMESPACE}" get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}')
	node=$(k -n "${SYSTEM_NAMESPACE}" get "pod/${pod}" -o jsonpath='{.spec.nodeName}')
	if [[ "${node}" == "${SOURCE_NODE}" ]]; then
		k -n "${SYSTEM_NAMESPACE}" delete "pod/${pod}" --wait=true --timeout=120s
		k -n "${SYSTEM_NAMESPACE}" rollout status deployment/shiftpv-controller --timeout=300s
	fi
	node=$(k -n "${SYSTEM_NAMESPACE}" get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].spec.nodeName}')
	test "${node}" = "${DESTINATION_NODE}"
	k uncordon "${SOURCE_NODE}" >/dev/null
}

create_source_workload() {
	local namespace=$1 payload=$2 pod
	if k get "namespace/${namespace}" >/dev/null 2>&1; then
		echo "test namespace already exists: ${namespace}" >&2
		return 1
	fi
	k cordon "${DESTINATION_NODE}" >/dev/null
	k apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${namespace}
  labels:
    shiftpv.io/admission: enabled
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: ${namespace}
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  storageClassName: ${STORAGE_CLASS}
  resources:
    requests:
      storage: 512Mi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: writer
  namespace: ${namespace}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${namespace}
  template:
    metadata:
      labels:
        app: ${namespace}
    spec:
      containers:
        - name: writer
          image: ${WORKLOAD_IMAGE}
          command: [sh, -ec]
          args:
            - |
              if [ ! -f /data/payload ]; then
                dd if=/dev/zero of=/data/payload bs=1M count=256
                printf '%s\\n' '${payload}' >>/data/payload
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
	k -n "${namespace}" rollout status deployment/writer --timeout=600s
	k -n "${namespace}" wait pvc/data --for=jsonpath='{.status.phase}'=Bound --timeout=180s
	pod=$(k -n "${namespace}" get pod -l "app=${namespace}" -o jsonpath='{.items[0].metadata.name}')
	test "$(k -n "${namespace}" get "pod/${pod}" -o jsonpath='{.spec.nodeName}')" = "${SOURCE_NODE}"
	PVC_UID=$(k -n "${namespace}" get pvc/data -o jsonpath='{.metadata.uid}')
	PV_NAME=$(k -n "${namespace}" get pvc/data -o jsonpath='{.spec.volumeName}')
	VOLUME_ID=$(k get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
	SOURCE_CHECKSUM=$(k -n "${namespace}" exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')
	SOURCE_POOL=$(k get shiftpvpools -o jsonpath="{.items[?(@.spec.nodeName=='${SOURCE_NODE}')].spec.mountPath}")
	DESTINATION_POOL=$(k get shiftpvpools -o jsonpath="{.items[?(@.spec.nodeName=='${DESTINATION_NODE}')].spec.mountPath}")
	test -n "${SOURCE_POOL}"
	test -n "${DESTINATION_POOL}"
	test "$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
	k uncordon "${DESTINATION_NODE}" >/dev/null
}

trigger_move() {
	local deadline=$((SECONDS + PHASE_TIMEOUT_SECONDS))
	MOVE_NAME=''
	k cordon "${SOURCE_NODE}" >/dev/null
	while ((SECONDS < deadline)); do
		MOVE_NAME=$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}' 2>/dev/null || true)
		[[ -n "${MOVE_NAME}" ]] && return
		sleep 1
	done
	echo "move was not created for ${VOLUME_ID}" >&2
	return 1
}

wait_for_phase() {
	local expected=$1 deadline=$((SECONDS + PHASE_TIMEOUT_SECONDS)) phase=''
	while ((SECONDS < deadline)); do
		phase=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		[[ "${phase}" == "${expected}" ]] && return
		if [[ "${phase}" == Blocked || "${phase}" == Succeeded ]]; then
			echo "move reached terminal phase before ${expected}: ${phase}" >&2
			return 1
		fi
		sleep 0.1
	done
	echo "move did not reach ${expected}; phase=${phase}" >&2
	return 1
}

assert_identity_and_checksum() {
	local namespace=$1 expected_node=$2 pod checksum
	k -n "${namespace}" rollout status deployment/writer --timeout=600s
	pod=$(k -n "${namespace}" get pod -l "app=${namespace}" -o jsonpath='{.items[0].metadata.name}')
	test "$(k -n "${namespace}" get "pod/${pod}" -o jsonpath='{.spec.nodeName}')" = "${expected_node}"
	checksum=$(k -n "${namespace}" exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')
	test "${checksum}" = "${SOURCE_CHECKSUM}"
	test "$(k -n "${namespace}" get pvc/data -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
	test "$(k -n "${namespace}" get pvc/data -o jsonpath='{.spec.volumeName}')" = "${PV_NAME}"
	test "$(k get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')" = "${VOLUME_ID}"
}

cleanup_case() {
	local namespace=$1 deadline phase='' recovery_phase='' cleanup_phase='' capacity_approved='' capacity_reason='' finalizers=''
	k patch "pv/${PV_NAME}" --type=merge -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
	k delete "namespace/${namespace}" --wait=true --timeout=300s
	k wait "pv/${PV_NAME}" --for=delete --timeout=300s
	k wait "shiftpvvolume/${VOLUME_ID}" --for=delete --timeout=300s
	deadline=$((SECONDS + 180))
	while ((SECONDS < deadline)); do
		phase=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		recovery_phase=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.recoveryPhase}' 2>/dev/null || true)
		cleanup_phase=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.cleanup.status.phase}' 2>/dev/null || true)
		capacity_approved=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.capacityApproved}' 2>/dev/null || true)
		capacity_reason=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.capacityReason}' 2>/dev/null || true)
		finalizers=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.metadata.finalizers}' 2>/dev/null || true)
		if [[ -z "${finalizers}" ]] &&
			{ [[ "${phase}" == Succeeded && "${cleanup_phase}" == Completed ]] ||
				[[ "${phase}" == Blocked && "${recovery_phase}" == Recovered && "${capacity_approved}" != true && "${capacity_reason}" == RecoverySettled ]]; }; then
			printf 'PASS retained settled Move journal move=%s phase=%s recovery=%s cleanup=%s\n' \
				"${MOVE_NAME}" "${phase}" "${recovery_phase:-none}" "${cleanup_phase:-none}"
			break
		fi
		sleep 2
	done
	if [[ -n "${finalizers}" ]] ||
		{ [[ "${phase}" != Succeeded || "${cleanup_phase}" != Completed ]] &&
			[[ "${phase}" != Blocked || "${recovery_phase}" != Recovered || "${capacity_approved}" == true || "${capacity_reason}" != RecoverySettled ]]; }; then
		echo "Move journal did not settle: move=${MOVE_NAME} phase=${phase} recovery=${recovery_phase} cleanup=${cleanup_phase} capacityApproved=${capacity_approved} capacityReason=${capacity_reason} finalizers=${finalizers}" >&2
		return 1
	fi
	k uncordon "${SOURCE_NODE}" >/dev/null
	k uncordon "${DESTINATION_NODE}" >/dev/null
}

run_precommit_source_interruption() {
	local namespace="${TEST_PREFIX}-precommit" reason
	create_source_workload "${namespace}" 'ShiftPV real-node precommit source recovery'
	trigger_move
	wait_for_phase Copying
	stop_source_node
	k wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.phase}'=Blocked --timeout=300s
	reason=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.reason}')
	test "${reason}" = SourceUnavailable
	test "$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
	ssh_source sudo test -f "${SOURCE_POOL}/volumes/${VOLUME_ID}/payload"
	start_source_node
	k uncordon "${SOURCE_NODE}" >/dev/null
	k patch "shiftpvmove/${MOVE_NAME}" --type=merge -p '{"spec":{"recovery":"ResumeOwner"}}'
	k wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.recoveryPhase}'=Recovered --timeout=600s
	assert_identity_and_checksum "${namespace}" "${SOURCE_NODE}"
	test "$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = Ready
	test "$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
	ssh_destination sudo test ! -e "${DESTINATION_POOL}/volumes/${VOLUME_ID}"
	printf 'PASS precommit source recovery volume=%s move=%s checksum=%s\n' "${VOLUME_ID}" "${MOVE_NAME}" "${SOURCE_CHECKSUM}"
	cleanup_case "${namespace}"
}

stop_after_commit_before_cleanup() {
	local deadline=$((SECONDS + PHASE_TIMEOUT_SECONDS)) phase='' owner=''
	while ((SECONDS < deadline)); do
		phase=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		owner=$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}' 2>/dev/null || true)
		if [[ "${owner}" == "${DESTINATION_NODE}" && ("${phase}" == Committing || "${phase}" == WaitingForDestinationPublish) ]]; then
			stop_source_node
			return
		fi
		if [[ "${phase}" == CleaningSource || "${phase}" == Completing || "${phase}" == Succeeded || "${phase}" == Blocked ]]; then
			echo "missed the postcommit pre-cleanup boundary: phase=${phase} owner=${owner}" >&2
			return 1
		fi
		sleep 0.1
	done
	echo "move did not commit before deadline: phase=${phase} owner=${owner}" >&2
	return 1
}

run_postcommit_cleanup_interruption() {
	local namespace="${TEST_PREFIX}-cleanup" deadline cleanup_phase='' phase=''
	create_source_workload "${namespace}" 'ShiftPV real-node postcommit cleanup recovery'
	trigger_move
	stop_after_commit_before_cleanup
	test "$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${DESTINATION_NODE}"
	ssh_source sudo test -f "${SOURCE_POOL}/volumes/${VOLUME_ID}/payload"
	deadline=$((SECONDS + 300))
	while ((SECONDS < deadline)); do
		phase=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		cleanup_phase=$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.cleanup.status.phase}' 2>/dev/null || true)
		if [[ ("${phase}" == WaitingForDestinationPublish || "${phase}" == CleaningSource) && ("${cleanup_phase}" == Pending || "${cleanup_phase}" == Running) ]]; then
			break
		fi
		if [[ "${cleanup_phase}" == Completed || "${cleanup_phase}" == NeedsReview || "${phase}" == Succeeded || "${phase}" == Blocked ]]; then
			echo "cleanup crossed an unsafe terminal boundary while ${SOURCE_NODE} was unavailable: phase=${phase} cleanup=${cleanup_phase}" >&2
			return 1
		fi
		sleep 1
	done
	if [[ "${cleanup_phase}" != Pending && "${cleanup_phase}" != Running ]]; then
		echo "cleanup did not enter a retryable state: phase=${phase} cleanup=${cleanup_phase}" >&2
		return 1
	fi
	start_source_node
	k wait "shiftpvmove/${MOVE_NAME}" --for=jsonpath='{.status.phase}'=Succeeded --timeout=900s
	assert_identity_and_checksum "${namespace}" "${DESTINATION_NODE}"
	test "$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = Ready
	test "$(k get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${DESTINATION_NODE}"
	test "$(k get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.cleanup.status.phase}')" = Completed
	ssh_source sudo test ! -e "${SOURCE_POOL}/volumes/${VOLUME_ID}"
	ssh_destination sudo test -f "${DESTINATION_POOL}/volumes/${VOLUME_ID}/payload"
	printf 'PASS postcommit cleanup recovery volume=%s move=%s checksum=%s\n' "${VOLUME_ID}" "${MOVE_NAME}" "${SOURCE_CHECKSUM}"
	cleanup_case "${namespace}"
}

"${ROOT_DIR}/test/e2e/real-node/preflight.sh" | tee "${ARTIFACT_DIR}/preflight.txt"
snapshot_non_shiftpv_specs before prefix "${TEST_PREFIX}-"
capture_evidence before source-kubelite
ensure_controller_on_destination
run_precommit_source_interruption
run_postcommit_cleanup_interruption

k wait shiftpvpool --all --for=condition=Ready --timeout=300s
test "$(k get shiftpvvolumes -o json | jq '.items | length')" = 0
assert_no_unsettled_moves
snapshot_non_shiftpv_specs after prefix "${TEST_PREFIX}-"
for subject in non-shiftpv-workloads existing-pvcs existing-pvs storageclasses; do
	diff -u "${ARTIFACT_DIR}/before-${subject}.json" "${ARTIFACT_DIR}/after-${subject}.json" >"${ARTIFACT_DIR}/${subject}.diff"
done
capture_evidence passed source-kubelite

printf '%s context=%s source=%s destination=%s artifacts=%s\n' \
	"${RESULT_MARKER}" "${KUBECTL_CONTEXT}" "${SOURCE_NODE}" "${DESTINATION_NODE}" "${ARTIFACT_DIR}"
