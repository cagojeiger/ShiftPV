#!/usr/bin/env bash

# Files inside a kind node may be owned by root and deliberately inaccessible
# to the host runner. Inspect them through the node container so the assertions
# exercise the same filesystem and permission boundary as ShiftPV.
assert_node_file() {
	local node=$1 path=$2
	if docker exec "${node}" test -f "${path}"; then
		return
	fi
	echo "expected file is missing inside kind node: node=${node} path=${path}" >&2
	docker exec "${node}" ls -lad "$(dirname "${path}")" >&2 2>/dev/null || true
	return 1
}

assert_node_absent() {
	local node=$1 path=$2
	if docker exec "${node}" test ! -e "${path}"; then
		return
	fi
	echo "unexpected path remains inside kind node: node=${node} path=${path}" >&2
	docker exec "${node}" ls -lad "${path}" >&2 2>/dev/null || true
	return 1
}

node_sha256() {
	local node=$1 path=$2
	docker exec "${node}" sha256sum "${path}" | awk '{print $1}'
}
