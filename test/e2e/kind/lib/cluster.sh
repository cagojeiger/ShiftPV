#!/usr/bin/env bash
# Shared Kind cluster lifecycle helpers for the suite entry points. This file is
# sourced.

# The pinned node image lives in the public-artifact lock so every suite and the
# published-artifact smoke agree on one Kubernetes version. Only that one key is
# read: sourcing the lock would also define NODE_IMAGE, which the Kind entry
# points already use for the node image they boot.
kind_node_image() {
	local lock image
	lock="$(cd "$(dirname "${BASH_SOURCE[0]}")/../artifact" && pwd)/versions.env"
	image=$(awk -F= '$1 == "KIND_NODE_IMAGE" { print $2 }' "${lock}")
	[[ -n "${image}" ]] || {
		echo "KIND_NODE_IMAGE not found in ${lock}" >&2
		return 1
	}
	printf '%s\n' "${image}"
}

# Every run owns its cluster and host directories and removes exactly those on
# success or failure. KEEP_CLUSTER=1 is the bounded-diagnosis escape hatch.
# Requires CLUSTER_NAME, KUBECONFIG, WORK_DIR and KEEP_CLUSTER from the caller.
delete_cluster_unless_kept() {
	if [[ "${KEEP_CLUSTER}" == "1" ]]; then
		echo "keeping cluster ${CLUSTER_NAME}, kubeconfig ${KUBECONFIG}, and data under ${WORK_DIR}"
		return
	fi
	kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
	rm -rf -- "${WORK_DIR}"
}
