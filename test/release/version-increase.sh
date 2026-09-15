#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
assert_increase="${repo_root}/build/ci/assert-version-increase.sh"

for pair in "0.4.1 0.4.0" "0.5.0 0.4.99" "1.0.0 0.99.99" "10.0.0 9.99.99"; do
	read -r new_version previous_version <<<"${pair}"
	"${assert_increase}" "${new_version}" "${previous_version}"
done

for pair in "0.4.0 0.4.0" "0.3.9 0.4.0" "0.4.9 0.5.0" "1.9.9 2.0.0"; do
	read -r new_version previous_version <<<"${pair}"
	if "${assert_increase}" "${new_version}" "${previous_version}" >/dev/null 2>&1; then
		echo "expected ${new_version} after ${previous_version} to be rejected" >&2
		exit 1
	fi
done

for pair in "v0.4.1 0.4.0" "0.4 0.3.0" "0.4.1-rc.1 0.4.0"; do
	read -r new_version previous_version <<<"${pair}"
	if "${assert_increase}" "${new_version}" "${previous_version}" >/dev/null 2>&1; then
		echo "expected invalid version ${new_version} to be rejected" >&2
		exit 1
	fi
done

echo "version increase tests passed"
