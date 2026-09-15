#!/usr/bin/env bash
# Shared helpers for the real-node qualification scripts. Source this after
# ROOT_DIR is set and before any helper below is used; every helper reads its
# configuration at call time, so the sourcing script may still set its own
# defaults afterwards.

k() {
	kubectl --context "${KUBECTL_CONTEXT}" --request-timeout=30s "$@"
}

# The fault stages refuse to start unless the reviewed identity of the cluster,
# both nodes, and both candidate images is supplied in full.
require_real_node_env() {
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
}

require_real_node_commands() {
	local command
	for command in kubectl jq ssh diff tee; do
		command -v "${command}" >/dev/null || {
			echo "required command not found: ${command}" >&2
			exit 1
		}
	done
}

# Defaults shared by every fault stage. ARTIFACT_DIR stays with the caller
# because each stage names its own run directory.
apply_real_node_defaults() {
	SYSTEM_NAMESPACE=${SYSTEM_NAMESPACE:-shiftpv-system}
	STORAGE_CLASS=${STORAGE_CLASS:-shiftpv}
	SSH_CONNECT_TIMEOUT=${SSH_CONNECT_TIMEOUT:-5}
	WORKLOAD_IMAGE=${WORKLOAD_IMAGE:-busybox:1.37@sha256:7a3ebe5bfd1a4a19797d20b0c0bb39d44393e9a03fd852c0865b0f540d868df0}
	# shellcheck disable=SC2034 # the sourcing stage builds ARTIFACT_DIR from it.
	RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
}

report_error() {
	local result_code=$?
	trap - ERR
	printf 'FAIL line=%s exit=%d command=%s\n' "${BASH_LINENO[0]}" "${result_code}" "${BASH_COMMAND}" |
		tee -a "${ARTIFACT_DIR}/failure.txt" >&2
	exit "${result_code}"
}

# Pass the optional second argument source-kubelite to additionally collect the
# source host kubelite journal; only stages that define ssh_source may ask for
# it.
capture_evidence() {
	local label=$1 extra=${2:-}
	k get nodes -o wide >"${ARTIFACT_DIR}/${label}-nodes.txt" 2>&1 || true
	k get pods -A -o wide >"${ARTIFACT_DIR}/${label}-pods.txt" 2>&1 || true
	k get pvc,pv -A -o wide >"${ARTIFACT_DIR}/${label}-storage.txt" 2>&1 || true
	k get shiftpvpools,shiftpvvolumes,shiftpvmoves -o yaml >"${ARTIFACT_DIR}/${label}-shiftpv.yaml" 2>&1 || true
	k get events -A --sort-by=.metadata.creationTimestamp >"${ARTIFACT_DIR}/${label}-events.txt" 2>&1 || true
	k -n "${SYSTEM_NAMESPACE}" logs deployment/shiftpv-controller --all-containers --tail=-1 >"${ARTIFACT_DIR}/${label}-controller.log" 2>&1 || true
	if [[ "${extra}" == source-kubelite ]]; then
		ssh_source sudo journalctl -u snap.microk8s.daemon-kubelite --since '-1 hour' --no-pager >"${ARTIFACT_DIR}/${label}-${SOURCE_NODE}-kubelite.log" 2>&1 || true
	fi
}

# The stages own their test namespaces differently: pass match=prefix with a
# namespace prefix, or match=exact with a single namespace name. Everything the
# run does not own must be byte-identical before and after.
snapshot_non_shiftpv_specs() {
	local label=$1 match=$2 value=$3 autoscaled_workloads
	autoscaled_workloads=$(k get horizontalpodautoscalers.autoscaling -A -o json | jq -c '
		[.items[] | {namespace: .metadata.namespace, kind: .spec.scaleTargetRef.kind, name: .spec.scaleTargetRef.name}]')
	k get deployments.apps,statefulsets.apps -A -o json | jq -S \
		--arg system "${SYSTEM_NAMESPACE}" --arg match "${match}" --arg value "${value}" \
		--argjson autoscaled "${autoscaled_workloads}" '
		def is_test_ns($ns): if $match == "prefix" then ($ns | startswith($value)) else $ns == $value end;
		[.items[]
		 | select(.metadata.namespace != $system)
		 | select(is_test_ns(.metadata.namespace) | not)
		 | . as $workload
		 | {apiVersion, kind, metadata: {namespace: .metadata.namespace, name: .metadata.name, uid: .metadata.uid},
		    spec: (if any($autoscaled[];
		      .namespace == $workload.metadata.namespace and
		      .kind == $workload.kind and
		      .name == $workload.metadata.name)
		    then ($workload.spec | del(.replicas))
		    else $workload.spec end)}]
		| sort_by(.apiVersion, .kind, .metadata.namespace, .metadata.name)' >"${ARTIFACT_DIR}/${label}-non-shiftpv-workloads.json"
	k get pvc -A -o json | jq -S --arg match "${match}" --arg value "${value}" '
		def is_test_ns($ns): if $match == "prefix" then ($ns | startswith($value)) else $ns == $value end;
		[.items[]
		 | select(is_test_ns(.metadata.namespace) | not)
		 | {metadata: {namespace: .metadata.namespace, name: .metadata.name, uid: .metadata.uid}, spec}]
		| sort_by(.metadata.namespace, .metadata.name)' >"${ARTIFACT_DIR}/${label}-existing-pvcs.json"
	k get pv -o json | jq -S --arg match "${match}" --arg value "${value}" '
		def is_test_ns($ns): if $match == "prefix" then ($ns | startswith($value)) else $ns == $value end;
		[.items[]
		 | select(is_test_ns(.spec.claimRef.namespace // "") | not)
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
