#!/usr/bin/env bash
# Shared polling helpers for the Kind suites. This file is sourced.

# Mobility discovery is asynchronous, so every suite that provokes a move has to
# wait for the ShiftPVMove object to exist before it can assert anything about
# it. Print the Move name for the volume, or fail once the timeout expires.
wait_for_move() {
	local volume_id=$1 timeout=$2 interval=${3:-1}
	local deadline=$((SECONDS + timeout)) move=""
	while ((SECONDS < deadline)); do
		move=$(kubectl get shiftpvmoves -o jsonpath="{.items[?(@.spec.volumeID=='${volume_id}')].metadata.name}" 2>/dev/null || true)
		if [[ -n "${move}" ]]; then
			printf '%s\n' "${move}"
			return
		fi
		sleep "${interval}"
	done
	echo "Move was not created for ${volume_id}" >&2
	return 1
}
