#!/bin/sh
set -eu

if [ "${1:-}" = copy ] && [ -f /pool/.shiftpv-e2e-fail-copy ]; then
	echo 'injected copy failure before filesystem mutation' >&2
	exit 97
fi
if [ "${1:-}" = cleanup ] && [ -f /pool/.shiftpv-e2e-fail-cleanup ]; then
	echo 'injected cleanup failure before filesystem mutation' >&2
	exit 98
fi

exec /shiftpv-volume-helper-real "$@"
