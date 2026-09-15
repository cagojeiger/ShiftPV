#!/usr/bin/env bash
# Sourced by recovery.sh; all storage belongs to the isolated mobility cluster.

verify_cleanup_lifecycle() {
	local move=$1 node=$2 root=$3 checksum=$4 pod=$5
	local source_copy cleanup_job controller_uid denial deadline release_revision release_status
	test "$(kubectl config current-context)" = "kind-${CLUSTER_NAME}"
	source_copy=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.sourceCopy.copyID}')
	test -n "${source_copy}"
	test "$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.cleanup.status.phase}')" = NeedsReview
	test "$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.cleanup.spec.authority.name}')" = "${move}"
	test "$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.cleanup.spec.target.copyID}')" = "${source_copy}"
	docker exec "${node}" test -f "${root}/volumes/${VOLUME_ID}/payload"

	# A permanent helper failure converges to review and never replays by itself.
	cleanup_job=$(cleanup_job_name "shiftpvmove/${move}")
	test -n "${cleanup_job}"
	kubectl -n shiftpv-system delete "job/${cleanup_job}" --ignore-not-found --wait=true --timeout=120s
	controller_uid=$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.uid}')
	kubectl -n shiftpv-system delete pod -l app.kubernetes.io/component=controller --wait=true --timeout=120s
	kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=180s
	test "${controller_uid}" != "$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.uid}')"
	deadline=$((SECONDS + 45))
	while ((SECONDS < deadline)); do
		test "$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.cleanup.status.phase}')" = NeedsReview
		test -z "$(kubectl -n shiftpv-system get "job/${cleanup_job}" --ignore-not-found -o name)"
		docker exec "${node}" test -f "${root}/volumes/${VOLUME_ID}/payload"
		sleep 5
	done

	test "$(pod_sha256 shiftpv-mobility-test "${pod}" /data/payload)" = "${checksum}"
	test "$(kubectl -n shiftpv-mobility-test get pvc/wffc -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
	test "$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.claimRef.uid}')" = "${PVC_UID}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = "${move}"
	test "$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.capacityApproved}')" = true
	test "$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.capacityReason}')" != RecoverySettled
	if denial=$(kubectl -n shiftpv-system delete deployment/shiftpv-controller --dry-run=server 2>&1); then
		echo 'NeedsReview cleanup permitted controller deletion' >&2
		return 1
	fi
	grep -Fq 'ShiftPVMove' <<<"${denial}"
	grep -Fq "${move}" <<<"${denial}"
	release_revision=$(helm status shiftpv --namespace shiftpv-system -o json | jq -r '.version')
	if helm uninstall shiftpv --namespace shiftpv-system --timeout 2m >"${WORK_DIR}/cleanup-uninstall.txt" 2>&1; then
		echo 'NeedsReview cleanup permitted Helm uninstall' >&2
		return 1
	fi
	release_status=$(helm status shiftpv --namespace shiftpv-system -o json | jq -r '.info.status')
	test "${release_status}" = uninstalling
	kubectl -n shiftpv-system get deployment/shiftpv-controller >/dev/null
	kubectl -n shiftpv-system wait --for=delete configmap/shiftpv-uninstall-permit --timeout=30s
	kubectl -n shiftpv-system logs job/shiftpv-uninstall-guard | grep -Fq 'ShiftPVMove'
	kubectl -n shiftpv-system logs job/shiftpv-uninstall-guard | grep -Fq "${move}"
	helm rollback shiftpv "${release_revision}" --namespace shiftpv-system \
		--no-hooks --wait --timeout 5m
	test "$(helm status shiftpv --namespace shiftpv-system -o json | jq -r '.info.status')" = deployed
	echo "cleanup lifecycle passed: permanent failure preserved exact copy and NeedsReview survived restart; move=${move} copy=${source_copy} checksum=${checksum}"
}
