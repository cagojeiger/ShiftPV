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

printf '%s\n' 'unrelated change' >"${fixture}/README.md"
git -C "${fixture}" add README.md
git -C "${fixture}" commit -qm unrelated
unrelated_sha="$(git -C "${fixture}" rev-parse HEAD)"
skip_output="${fixture}/skip-output"
(
  cd "${fixture}"
  RELEASE_SHA="${unrelated_sha}" CURRENT_MAIN_SHA="${unrelated_sha}" GITHUB_OUTPUT="${skip_output}" \
    build/ci/resolve-chart-release.sh
)
[[ "$(sed -n 's/^should_release=//p' "${skip_output}")" == false ]]

printf '%s\n' 'changed chart' >"${fixture}/charts/shiftpv/README.md"
git -C "${fixture}" add charts
git -C "${fixture}" commit -qm changed-chart
changed_chart_sha="$(git -C "${fixture}" rev-parse HEAD)"
conflict_output="${fixture}/conflict-output"
if (
  cd "${fixture}"
  RELEASE_SHA="${changed_chart_sha}" CURRENT_MAIN_SHA="${changed_chart_sha}" GITHUB_OUTPUT="${conflict_output}" \
    build/ci/resolve-chart-release.sh
); then
  echo "expected changed chart content without a version bump to reject publication" >&2
  exit 1
fi

printf '%s\n' 'version: 0.2.0-rc.1' >"${fixture}/charts/shiftpv/Chart.yaml"
git -C "${fixture}" add charts/shiftpv/Chart.yaml
git -C "${fixture}" commit -qm invalid-version
invalid_sha="$(git -C "${fixture}" rev-parse HEAD)"
invalid_output="${fixture}/invalid-output"
if (
  cd "${fixture}"
  RELEASE_SHA="${invalid_sha}" CURRENT_MAIN_SHA="${invalid_sha}" GITHUB_OUTPUT="${invalid_output}" \
    build/ci/resolve-chart-release.sh
); then
  echo "expected a non-numeric chart version to be rejected" >&2
  exit 1
fi

echo "chart release resolver tests passed"
