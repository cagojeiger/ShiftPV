#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT

git -C "${fixture}" init -q
git -C "${fixture}" config user.name shiftpv-test
git -C "${fixture}" config user.email shiftpv-test@example.invalid
mkdir -p "${fixture}/build/ci" "${fixture}/versions"
cp "${repo_root}/build/ci/resolve-image-release.sh" "${fixture}/build/ci/"
printf '%s\n' 0.0.9 >"${fixture}/versions/controller"
printf '%s\n' 0.1.0 >"${fixture}/versions/node"
git -C "${fixture}" add build versions
git -C "${fixture}" commit -qm base
base_sha="$(git -C "${fixture}" rev-parse HEAD)"
git -C "${fixture}" tag node/v0.1.0

printf '%s\n' 0.1.0 >"${fixture}/versions/controller"
git -C "${fixture}" add versions/controller
git -C "${fixture}" commit -qm controller-release
release_sha="$(git -C "${fixture}" rev-parse HEAD)"

output="${fixture}/output"
(
  cd "${fixture}"
  RELEASE_SHA="${release_sha}" CURRENT_MAIN_SHA="${release_sha}" GITHUB_OUTPUT="${output}" \
    build/ci/resolve-image-release.sh
)

components="$(sed -n 's/^components=//p' "${output}")"
builds="$(sed -n 's/^builds=//p' "${output}")"
[[ "$(sed -n 's/^should_release=//p' "${output}")" == true ]]
jq -e 'length == 1 and .[0].component == "controller" and .[0].version == "0.1.0"' \
  <<<"${components}" >/dev/null
jq -e 'length == 2 and all(.[]; .component == "controller")' <<<"${builds}" >/dev/null

stale_output="${fixture}/stale-output"
(
  cd "${fixture}"
  RELEASE_SHA="${release_sha}" CURRENT_MAIN_SHA="${base_sha}" GITHUB_OUTPUT="${stale_output}" \
    build/ci/resolve-image-release.sh
)
[[ "$(sed -n 's/^should_release=//p' "${stale_output}")" == false ]]

git -C "${fixture}" tag controller/v0.1.0 "${base_sha}"
conflict_output="${fixture}/conflict-output"
if (
  cd "${fixture}"
  RELEASE_SHA="${release_sha}" CURRENT_MAIN_SHA="${release_sha}" GITHUB_OUTPUT="${conflict_output}" \
    build/ci/resolve-image-release.sh
); then
  echo "expected an existing tag on another commit to reject the release" >&2
  exit 1
fi

git -C "${fixture}" tag -d controller/v0.1.0 >/dev/null
printf '%s\n' 0.2.0-rc.1 >"${fixture}/versions/controller"
git -C "${fixture}" add versions/controller
git -C "${fixture}" commit -qm invalid-version
invalid_sha="$(git -C "${fixture}" rev-parse HEAD)"
invalid_output="${fixture}/invalid-output"
if (
  cd "${fixture}"
  RELEASE_SHA="${invalid_sha}" CURRENT_MAIN_SHA="${invalid_sha}" GITHUB_OUTPUT="${invalid_output}" \
    build/ci/resolve-image-release.sh
); then
  echo "expected a non-numeric release version to be rejected" >&2
  exit 1
fi

echo "release resolver tests passed"
