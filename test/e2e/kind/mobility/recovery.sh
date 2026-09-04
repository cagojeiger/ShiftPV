#!/usr/bin/env bash
# Sourced by run.sh: isolated kubeconfig and owned temporary pool directories only.

request_recovery() {
	local move=$1
	if kubectl patch "shiftpvmove/${move}" --type merge -p '{"spec":{"recovery":"ForceSource"}}' >"${WORK_DIR}/invalid-recovery.txt" 2>&1; then
		echo 'invalid recovery request was accepted' >&2
		return 1
	fi
	grep -q 'Unsupported value' "${WORK_DIR}/invalid-recovery.txt"
	kubectl patch "shiftpvmove/${move}" --type merge -p '{"spec":{"recovery":"ResumeOwner"}}'
	kubectl patch "shiftpvmove/${move}" --type merge -p '{"spec":{"recovery":"ResumeOwner"}}'
	if kubectl patch "shiftpvmove/${move}" --type merge -p '{"spec":{"recovery":null}}' >"${WORK_DIR}/removed-recovery.txt" 2>&1; then
		echo 'one-way recovery request was removed' >&2
		return 1
	fi
	grep -q 'recovery cannot be removed' "${WORK_DIR}/removed-recovery.txt"
}

restart_during_recovery() {
	local move=$1 pod
	kubectl wait "shiftpvmove/${move}" --for=jsonpath='{.status.recoveryPhase}'=Verifying --timeout=300s
	pod=$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}')
	# Observe process termination; do not create overlapping controllers.
	kubectl -n shiftpv-system delete "pod/${pod}" --wait=true --timeout=120s
	kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=180s
	kubectl wait "shiftpvmove/${move}" --for=jsonpath='{.status.recoveryPhase}'=Recovered --timeout=300s
	kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.message}' | grep -Fq 'no operator action is required'
	local recovery_event="" deadline=$((SECONDS + 60))
	while ((SECONDS < deadline)); do
		recovery_event=$(kubectl -n default get events \
			--field-selector "involvedObject.kind=ShiftPVMove,involvedObject.name=${move}" \
			-o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null || true)
		grep -Fxq RecoveryRecovered <<<"${recovery_event}" && return
		sleep 1
	done
	echo "missing ShiftPVMove RecoveryRecovered event; observed: ${recovery_event}" >&2
	return 1
}

recover_source_only() {
	local source_claim_uid checksum pod
	source_claim_uid=$(kubectl -n shiftpv-mobility-blocked get pvc/source-only -o jsonpath='{.metadata.uid}')
	checksum=$(shasum -a 256 "${WORKER_A_POOL}/volumes/${BLOCKED_VOLUME}/payload" | awk '{print $1}')
	request_recovery "${BLOCKED_MOVE}"
	restart_during_recovery "${BLOCKED_MOVE}"
	kubectl -n shiftpv-mobility-blocked rollout status deployment/source-only --timeout=180s
	pod=$(kubectl -n shiftpv-mobility-blocked get pod -l app=shiftpv-mobility-source-only -o jsonpath='{.items[0].metadata.name}')
	test "$(kubectl -n shiftpv-mobility-blocked exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')" = "${checksum}"
	test "$(kubectl -n shiftpv-mobility-blocked get pvc/source-only -o jsonpath='{.metadata.uid}')" = "${source_claim_uid}"
	test "$(kubectl get "shiftpvvolume/${BLOCKED_VOLUME}" -o jsonpath='{.status.ownerNode}')" = "${BLOCKED_SOURCE_NODE}"
	test "$(kubectl get "shiftpvvolume/${BLOCKED_VOLUME}" -o jsonpath='{.status.activeMove}')" = ""
	test "$(kubectl get "shiftpvmove/${BLOCKED_MOVE}" -o jsonpath='{.status.phase}')" = "Blocked"
	echo 'source recovery passed: original PVC and payload, same owner, restart and duplicate request'
	kubectl delete namespace shiftpv-mobility-blocked --wait=true --timeout=180s
}

recover_after_commit_failure() {
	local return_move current_pod latest_checksum failed_job
	# Real rename failure in source cleanup; the volume contents are untouched.
	kubectl uncordon "${SOURCE_NODE}"
	kubectl cordon "${DESTINATION_NODE}"
	return_move=""
	for _ in {1..120}; do
		return_move=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')
		[[ -n "${return_move}" ]] && break
		sleep 1
	done
	test -n "${return_move}"
	local fault_path="${DESTINATION_POOL}/.shiftpv/retired/${return_move}"
	mkdir -p "${DESTINATION_POOL}/.shiftpv/retired"
	test ! -e "${fault_path}"
	touch "${fault_path}"
	kubectl wait "shiftpvmove/${return_move}" --for=jsonpath='{.status.phase}'=Blocked --timeout=480s
	test "$(kubectl get "shiftpvmove/${return_move}" -o jsonpath='{.spec.sourceNode}')" = "${DESTINATION_NODE}"
	test "$(kubectl get "shiftpvmove/${return_move}" -o jsonpath='{.status.reason}')" = "CleanupFailed"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
	failed_job=$(kubectl get "shiftpvmove/${return_move}" -o jsonpath='{.status.cleanupJobName}')
	kubectl -n shiftpv-system logs "job/${failed_job}" >"${WORK_DIR}/cleanup-failure.txt" 2>&1
	kubectl -n shiftpv-mobility-test rollout status deployment/wffc --timeout=180s
	current_pod=$(kubectl -n shiftpv-mobility-test get pod -l app=shiftpv-mobility-wffc -o jsonpath='{.items[0].metadata.name}')
	# New writes on the committed destination must survive recovery.
	kubectl -n shiftpv-mobility-test exec "${current_pod}" -- sh -ec 'printf "after owner commit\n" >> /data/payload'
	latest_checksum=$(kubectl -n shiftpv-mobility-test exec "${current_pod}" -- sha256sum /data/payload | awk '{print $1}')
	test "${latest_checksum}" != "${CHECKSUM_BEFORE}"
	test -f "${fault_path}"
	rm -- "${fault_path}"
	request_recovery "${return_move}"
	restart_during_recovery "${return_move}"
	test "$(kubectl -n shiftpv-mobility-test exec "${current_pod}" -- sha256sum /data/payload | awk '{print $1}')" = "${latest_checksum}"
	test "$(kubectl -n shiftpv-mobility-test get pvc/wffc -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
	test "$(kubectl -n shiftpv-mobility-test get pvc/wffc -o jsonpath='{.spec.volumeName}')" = "${PV_NAME}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${SOURCE_NODE}"
	test -f "${DESTINATION_POOL}/.shiftpv/aborted/${return_move}-final/payload"
	test ! -e "${DESTINATION_POOL}/volumes/${VOLUME_ID}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = ""
	kubectl uncordon "${DESTINATION_NODE}"
	echo 'post-commit recovery passed: latest destination writes preserved; stale source quarantined'
}
