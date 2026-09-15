#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
	echo "usage: $0 NEW_VERSION PREVIOUS_VERSION" >&2
	exit 2
fi

new_version=$1
previous_version=$2
version_pattern='^[0-9]+\.[0-9]+\.[0-9]+$'

for version in "${new_version}" "${previous_version}"; do
	if ! [[ "${version}" =~ ${version_pattern} ]]; then
		echo "version must use numeric major.minor.patch format: ${version}" >&2
		exit 2
	fi
done

IFS=. read -r new_major new_minor new_patch <<<"${new_version}"
IFS=. read -r previous_major previous_minor previous_patch <<<"${previous_version}"
new_parts=("${new_major}" "${new_minor}" "${new_patch}")
previous_parts=("${previous_major}" "${previous_minor}" "${previous_patch}")

for index in 0 1 2; do
	if ((10#${new_parts[index]} > 10#${previous_parts[index]})); then
		exit 0
	fi
	if ((10#${new_parts[index]} < 10#${previous_parts[index]})); then
		break
	fi
done

echo "new version ${new_version} must be greater than previous version ${previous_version}" >&2
exit 1
