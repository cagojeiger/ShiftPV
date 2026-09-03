#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "${fixture}"' EXIT

git -C "${fixture}" init -q
git -C "${fixture}" config user.name shiftpv-test
git -C "${fixture}" config user.email shiftpv-test@example.invalid
mkdir -p "${fixture}/build/ci" "${fixture}/charts/shiftpv"
cp "${repo_root}/build/ci/resolve-chart-release.sh" "${fixture}/build/ci/"
printf '%s\n' 'version: 0.0.9' >"${fixture}/charts/shiftpv/Chart.yaml"
git -C "${fixture}" add build charts
git -C "${fixture}" commit -qm base
base_sha="$(git -C "${fixture}" rev-parse HEAD)"

printf '%s\n' 'version: 0.1.0' >"${fixture}/charts/shiftpv/Chart.yaml"
git -C "${fixture}" add charts
git -C "${fixture}" commit -qm chart-release
release_sha="$(git -C "${fixture}" rev-parse HEAD)"

output="${fixture}/output"
(
  cd "${fixture}"
  RELEASE_SHA="${release_sha}" CURRENT_MAIN_SHA="${release_sha}" GITHUB_OUTPUT="${output}" \
    build/ci/resolve-chart-release.sh
)
[[ "$(sed -n 's/^should_release=//p' "${output}")" == true ]]
[[ "$(sed -n 's/^version=//p' "${output}")" == 0.1.0 ]]
[[ "$(sed -n 's/^tag=//p' "${output}")" == chart/v0.1.0 ]]

stale_output="${fixture}/stale-output"
(
  cd "${fixture}"
  RELEASE_SHA="${release_sha}" CURRENT_MAIN_SHA=stale GITHUB_OUTPUT="${stale_output}" \
    build/ci/resolve-chart-release.sh
)
[[ "$(sed -n 's/^should_release=//p' "${stale_output}")" == false ]]

git -C "${fixture}" tag chart/v0.1.0
resume_output="${fixture}/resume-output"
(
  cd "${fixture}"
  RELEASE_SHA="${release_sha}" CURRENT_MAIN_SHA="${release_sha}" GITHUB_OUTPUT="${resume_output}" \
    build/ci/resolve-chart-release.sh
)
[[ "$(sed -n 's/^should_release=//p' "${resume_output}")" == true ]]

git -C "${fixture}" tag -d chart/v0.1.0 >/dev/null
git -C "${fixture}" tag chart/v0.1.0 "${base_sha}"
conflict_output="${fixture}/conflict-output"
if (
  cd "${fixture}"
  RELEASE_SHA="${release_sha}" CURRENT_MAIN_SHA="${release_sha}" GITHUB_OUTPUT="${conflict_output}" \
    build/ci/resolve-chart-release.sh
); then
  echo "expected a chart tag on another commit to reject publication" >&2
  exit 1
fi

printf '%s\n' 'version: 0.2.0-rc.1' >"${fixture}/charts/shiftpv/Chart.yaml"
invalid_output="${fixture}/invalid-output"
if (
  cd "${fixture}"
  RELEASE_SHA="${release_sha}" CURRENT_MAIN_SHA="${release_sha}" GITHUB_OUTPUT="${invalid_output}" \
    build/ci/resolve-chart-release.sh
); then
  echo "expected a non-numeric chart version to be rejected" >&2
  exit 1
fi

echo "chart release resolver tests passed"
