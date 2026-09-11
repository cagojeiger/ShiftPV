#!/usr/bin/env bash

# Files inside a kind node may be owned by root and deliberately inaccessible
# to the host runner. Inspect them through the node container so the assertions
# exercise the same filesystem and permission boundary as ShiftPV.
node_path_exists() {
	local node=$1 path=$2 running
	running=$(docker inspect -f '{{.State.Running}}' "${node}")
	if [[ "${running}" == true ]]; then
		docker exec "${node}" test -e "${path}"
		return
	fi
	# `docker cp ... -` reads a stopped container's writable layer without
	# starting the node and emits an archive only when the path exists.
	docker cp "${node}:${path}" - >/dev/null 2>&1
}

assert_node_file() {
	local node=$1 path=$2
	if node_path_exists "${node}" "${path}"; then
		return
	fi
	echo "expected file is missing inside kind node: node=${node} path=${path}" >&2
	if [[ "$(docker inspect -f '{{.State.Running}}' "${node}" 2>/dev/null || true)" == true ]]; then
		docker exec "${node}" ls -lad "$(dirname "${path}")" >&2 2>/dev/null || true
	fi
	return 1
}

assert_node_absent() {
	local node=$1 path=$2
	if ! node_path_exists "${node}" "${path}"; then
		return
	fi
	echo "unexpected path remains inside kind node: node=${node} path=${path}" >&2
	if [[ "$(docker inspect -f '{{.State.Running}}' "${node}" 2>/dev/null || true)" == true ]]; then
		docker exec "${node}" ls -lad "${path}" >&2 2>/dev/null || true
	fi
	return 1
}

node_sha256() {
	local node=$1 path=$2
	docker exec "${node}" sha256sum "${path}" | awk '{print $1}'
}
