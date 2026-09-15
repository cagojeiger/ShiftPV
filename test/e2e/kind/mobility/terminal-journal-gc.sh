#!/usr/bin/env bash
# Sourced by run.sh: isolated kubeconfig and owned temporary pool directories only.

verify_terminal_move_journal_gc() {
	local move=$1 volume=$2 pod=$3 owner_node=$4 owner_root=$5 expected_checksum=$6
	local move_uid deadline finalizers
	test "$(kubectl config current-context)" = "kind-${CLUSTER_NAME}"
	test "$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.phase}')" = Succeeded
	test "$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.cleanup.status.phase}')" = Completed
	test "$(kubectl get "shiftpvvolume/${volume}" -o jsonpath='{.status.activeMove}')" = ""

	deadline=$((SECONDS + 120))
	while ((SECONDS < deadline)); do
		finalizers=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.metadata.finalizers[*]}')
		[[ -z "${finalizers}" ]] && break
		sleep 1
	done
	if [[ -n "${finalizers}" ]]; then
		echo "terminal Move finalizers were not released: ${move} ${finalizers}" >&2
		return 1
	fi

	move_uid=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.metadata.uid}')
	test -n "${move_uid}"
	# Only the isolated test changes terminal time. The public chart retains real
	# journals for seven days; this makes the already-settled journal eligible now.
	kubectl patch "shiftpvmove/${move}" --subresource=status --type=merge \
		-p '{"status":{"lastTransitionTime":"2020-01-01T00:00:00Z"}}'
	kubectl wait "shiftpvmove/${move}" --for=delete --timeout=120s

	test "$(kubectl get "shiftpvvolume/${volume}" -o jsonpath='{.status.phase}')" = Ready
	test "$(kubectl get "shiftpvvolume/${volume}" -o jsonpath='{.status.ownerNode}')" = "${owner_node}"
	test "$(kubectl get "shiftpvvolume/${volume}" -o jsonpath='{.status.activeMove}')" = ""
	test "$(pod_sha256 shiftpv-mobility-test "${pod}" /data/payload)" = "${expected_checksum}"
	assert_node_file "${owner_node}" "${owner_root}/volumes/${volume}/payload"
	echo "terminal Move journal GC passed: metadata ${move}/${move_uid} deleted; serving copy and checksum retained"
}
