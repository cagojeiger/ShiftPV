#!/usr/bin/env bash
# Sourced by recovery.sh; all storage belongs to the isolated mobility cluster.

verify_cleanup_lifecycle() {
	local move=$1 node=$2 root=$3 checksum=$4 pod=$5
	local request source_copy controller_uid denial deadline release_revision release_status
	test "$(kubectl config current-context)" = "kind-${CLUSTER_NAME}"
	request=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.cleanupName}')
	source_copy=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.sourceCopy.copyID}')
	test -n "${request}"
	test -n "${source_copy}"
	test "$(kubectl get "shiftpvcleanup/${request}" -o jsonpath='{.status.phase}')" = NeedsReview
	test "$(kubectl get "shiftpvcleanup/${request}" -o jsonpath='{.spec.approved}')" = true
	test "$(kubectl get "shiftpvcleanup/${request}" -o jsonpath='{.spec.authority.name}')" = "${move}"
	test "$(kubectl get "shiftpvcleanup/${request}" -o jsonpath='{.spec.target.copyID}')" = "${source_copy}"
	docker exec "${node}" test -f "${root}/volumes/${VOLUME_ID}/payload"

	# A permanent helper failure converges to review and never replays by itself.
	kubectl -n shiftpv-system delete "job/${request}-effect" --ignore-not-found --wait=true --timeout=120s
	controller_uid=$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.uid}')
	kubectl -n shiftpv-system delete pod -l app.kubernetes.io/component=controller --wait=true --timeout=120s
	kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=180s
	test "${controller_uid}" != "$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.uid}')"
	deadline=$((SECONDS + 45))
	while ((SECONDS < deadline)); do
		test "$(kubectl get "shiftpvcleanup/${request}" -o jsonpath='{.status.phase}')" = NeedsReview
		test -z "$(kubectl -n shiftpv-system get "job/${request}-effect" --ignore-not-found -o name)"
		docker exec "${node}" test -f "${root}/volumes/${VOLUME_ID}/payload"
		sleep 5
	done

	test "$(kubectl -n shiftpv-mobility-test exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')" = "${checksum}"
	test "$(kubectl -n shiftpv-mobility-test get pvc/wffc -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
	test "$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.claimRef.uid}')" = "${PVC_UID}"
	test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = ""
	if denial=$(kubectl -n shiftpv-system delete deployment/shiftpv-controller --dry-run=server 2>&1); then
		echo 'NeedsReview cleanup permitted controller deletion' >&2
		return 1
	fi
	grep -Fq 'ShiftPVCleanup' <<<"${denial}"
	grep -Fq "${request}" <<<"${denial}"
	release_revision=$(helm status shiftpv --namespace shiftpv-system -o json | jq -r '.version')
	if helm uninstall shiftpv --namespace shiftpv-system --timeout 2m >"${WORK_DIR}/cleanup-uninstall.txt" 2>&1; then
		echo 'NeedsReview cleanup permitted Helm uninstall' >&2
		return 1
	fi
	release_status=$(helm status shiftpv --namespace shiftpv-system -o json | jq -r '.info.status')
	test "${release_status}" = uninstalling
	kubectl -n shiftpv-system get deployment/shiftpv-controller >/dev/null
	kubectl -n shiftpv-system wait --for=delete configmap/shiftpv-uninstall-permit --timeout=30s
	kubectl -n shiftpv-system logs job/shiftpv-uninstall-guard | grep -Fq 'ShiftPVCleanup'
	kubectl -n shiftpv-system logs job/shiftpv-uninstall-guard | grep -Fq "${request}"
	helm rollback shiftpv "${release_revision}" --namespace shiftpv-system \
		--no-hooks --wait --timeout 5m
	test "$(helm status shiftpv --namespace shiftpv-system -o json | jq -r '.info.status')" = deployed
	echo "cleanup lifecycle passed: permanent failure preserved exact copy and NeedsReview survived restart; request=${request} copy=${source_copy} checksum=${checksum}"
}
