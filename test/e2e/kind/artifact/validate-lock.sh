#!/usr/bin/env bash
set -euo pipefail

lock_file=${1:-"$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/versions.env"}
keys=(CHART_REPOSITORY CHART_VERSION CHART_SHA256 CONTROLLER_IMAGE NODE_IMAGE KIND_NODE_IMAGE)

[[ -f "${lock_file}" ]] || {
  echo "artifact lock not found: ${lock_file}" >&2
  exit 1
}

for key in "${keys[@]}"; do
  count=$(awk -F= -v key="${key}" '$1 == key { count++ } END { print count + 0 }' "${lock_file}")
  if [[ "${count}" != 1 ]]; then
    echo "artifact lock must contain exactly one ${key}; found ${count}" >&2
    exit 1
  fi
done

if awk -F= 'NF != 2 || $1 !~ /^[A-Z][A-Z0-9_]*$/ { exit 1 }' "${lock_file}"; then
  :
else
  echo "artifact lock contains malformed lines" >&2
  exit 1
fi
while IFS='=' read -r key _; do
  case "${key}" in
    CHART_REPOSITORY | CHART_VERSION | CHART_SHA256 | CONTROLLER_IMAGE | NODE_IMAGE | KIND_NODE_IMAGE) ;;
    *)
      echo "artifact lock contains unknown key: ${key}" >&2
      exit 1
      ;;
  esac
done <"${lock_file}"

chart_repository=$(awk -F= '$1 == "CHART_REPOSITORY" { print $2 }' "${lock_file}")
chart_version=$(awk -F= '$1 == "CHART_VERSION" { print $2 }' "${lock_file}")
chart_sha256=$(awk -F= '$1 == "CHART_SHA256" { print $2 }' "${lock_file}")
controller_image=$(awk -F= '$1 == "CONTROLLER_IMAGE" { print $2 }' "${lock_file}")
node_image=$(awk -F= '$1 == "NODE_IMAGE" { print $2 }' "${lock_file}")
kind_node_image=$(awk -F= '$1 == "KIND_NODE_IMAGE" { print $2 }' "${lock_file}")

[[ "${chart_repository}" =~ ^https://[A-Za-z0-9.-]+(/[A-Za-z0-9._~/-]*)?$ ]] || {
  echo "CHART_REPOSITORY must be a simple HTTPS URL" >&2
  exit 1
}
[[ "${chart_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "CHART_VERSION must use numeric major.minor.patch" >&2
  exit 1
}
[[ "${chart_sha256}" =~ ^[0-9a-f]{64}$ ]] || {
  echo "CHART_SHA256 must be a lowercase SHA-256 digest" >&2
  exit 1
}
image_pattern='^[a-z0-9.-]+(/[a-z0-9._-]+)+:[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}$'
[[ "${controller_image}" =~ ${image_pattern} ]] || {
  echo "CONTROLLER_IMAGE must pin a numeric version tag and sha256 digest" >&2
  exit 1
}
[[ "${node_image}" =~ ${image_pattern} ]] || {
  echo "NODE_IMAGE must pin a numeric version tag and sha256 digest" >&2
  exit 1
}
[[ "${kind_node_image}" =~ ^kindest/node:v[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}$ ]] || {
  echo "KIND_NODE_IMAGE must pin a Kubernetes version and sha256 digest" >&2
  exit 1
}

echo "artifact lock valid: chart=${chart_version} controller=${controller_image} node=${node_image}"
