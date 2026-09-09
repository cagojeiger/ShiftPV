#!/usr/bin/env bash
# Sourced by recovery.sh; all storage belongs to the isolated mobility cluster.

verify_cleanup_lifecycle() {
	local move=$1 node=$2 root=$3 checksum=$4 pod=$5
	local request move_uid job job_uid reason evidence before_uid deadline
	test "$(kubectl config current-context)" = "kind-${CLUSTER_NAME}"
	move_uid=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.metadata.uid}')
	request="shiftpv-cleanup-$(printf '%s' "${move_uid}" | shasum -a 256 | cut -c 1-32)"
	test "$(kubectl -n shiftpv-system get "configmap/${request}" -o jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-completed}')" = ""

	# Recovery has quiesced old workers; a check must reject the retained data.
	kubectl -n shiftpv-system annotate "configmap/${request}" shiftpv.io/cleanup-check=1 --overwrite
	kubectl -n shiftpv-system wait "configmap/${request}" --for=jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-check-done}'=true --timeout=600s
	test "$(kubectl -n shiftpv-system get "configmap/${request}" -o jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-state}')" = NeedsReview
	job="shiftpv-check-$(printf '%s' "${move_uid}/1" | shasum -a 256 | cut -c 1-24)"
	kubectl -n shiftpv-system logs "job/${job}" | grep -F 'cleanup path still exists'
	job_uid=$(kubectl -n shiftpv-system get "job/${job}" -o jsonpath='{.metadata.uid}')
	test "${job_uid}" = "$(kubectl -n shiftpv-system get "configmap/${request}" -o jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-check-job-uid}')"
	test "$(kubectl -n shiftpv-system get "job/${job}" -o jsonpath='{.spec.template.spec.containers[0].volumeMounts[0].readOnly}')" = true
	docker exec "${node}" test -f "${root}/.shiftpv/aborted/${move}-final/payload"

	# A restart plus the same request must not replay a failed attempt.
	kubectl -n shiftpv-system delete "job/${job}" --wait=true --timeout=120s
	before_uid=$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.uid}')
	kubectl -n shiftpv-system delete pod -l app.kubernetes.io/component=controller --wait=true --timeout=120s
	kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=180s
	test "${before_uid}" != "$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.uid}')"
	kubectl -n shiftpv-system annotate "configmap/${request}" shiftpv.io/cleanup-check=1 --overwrite
	# Force a fresh observation of the request without starting a new check.
	kubectl -n shiftpv-system annotate "configmap/${request}" shiftpv.io/cleanup-check=0 --overwrite
	kubectl -n shiftpv-system wait "configmap/${request}" --for=jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-reason}'='cleanup check must be a positive increasing integer' --timeout=180s
	kubectl -n shiftpv-system annotate "configmap/${request}" shiftpv.io/cleanup-check=1 --overwrite
	# Hold the duplicate across a full 60s scan interval plus controller wake margin.
	deadline=$((SECONDS + 85))
	while ((SECONDS < deadline)); do
		test -z "$(kubectl -n shiftpv-system get "job/${job}" --ignore-not-found -o name)"
		test "$(kubectl -n shiftpv-system get "configmap/${request}" -o jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-check-id}')" = 1
		test "$(kubectl -n shiftpv-system get "configmap/${request}" -o jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-check-job-uid}')" = "${job_uid}"
		test "$(kubectl -n shiftpv-system get "configmap/${request}" -o jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-check-done}')" = true
		test "$(kubectl -n shiftpv-system get "configmap/${request}" -o jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-completed}')" = ""
		sleep 5
	done

	# Operator resolution moves only this test's quarantine outside the Pool.
	# Preserve the bytes for the independent final audit instead of erasing them.
	evidence="${root}/../cleanup-evidence-${move}"
	docker exec "${node}" test ! -e "${evidence}"
	docker exec "${node}" mv -- "${root}/.shiftpv/aborted/${move}-final" "${evidence}"
	kubectl -n shiftpv-system annotate "configmap/${request}" shiftpv.io/cleanup-check=2 --overwrite
	kubectl -n shiftpv-system wait "configmap/${request}" --for=jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-completed}'=true --timeout=600s
	test "$(kubectl -n shiftpv-system get "configmap/${request}" -o jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-state}')" = Completed
	test -n "$(kubectl -n shiftpv-system get "configmap/${request}" -o jsonpath='{.metadata.annotations.shiftpv\.io/cleanup-completed-at}')"
	job="shiftpv-check-$(printf '%s' "${move_uid}/2" | shasum -a 256 | cut -c 1-24)"
	kubectl -n shiftpv-system wait "job/${job}" --for=condition=Complete --timeout=60s
	test "$(kubectl -n shiftpv-system get "job/${job}" -o jsonpath='{.spec.ttlSecondsAfterFinished}')" = 600
	docker exec "${node}" test ! -e "${root}/volumes/${VOLUME_ID}"
	docker exec "${node}" test ! -e "${root}/.shiftpv/aborted/${move}-final"
	test "$(kubectl -n shiftpv-mobility-test exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')" = "${checksum}"
	test "$(kubectl -n shiftpv-mobility-test get pvc/wffc -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
	test "$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.claimRef.uid}')" = "${PVC_UID}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = ""
	if reason=$(kubectl -n shiftpv-system delete deployment/shiftpv-controller --dry-run=server 2>&1); then
		echo 'active workload must still block uninstall' >&2
		return 1
	fi
	grep -Fq 'ShiftPV resource deletion denied: dependent storage exists:' <<<"${reason}"
	grep -Fq "PersistentVolume ${PV_NAME}" <<<"${reason}"
	if grep -Fq "CleanupRequest shiftpv-system/${request}" <<<"${reason}"; then
		echo 'completed cleanup still blocks uninstall' >&2
		return 1
	fi
	echo "cleanup lifecycle passed: retained bytes rejected; failed attempt not replayed; verified absence acknowledged; request=${request} evidence=${evidence} checksum=${checksum}"
}
