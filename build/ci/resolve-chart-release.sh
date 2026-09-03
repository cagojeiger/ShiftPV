#!/usr/bin/env bash
set -euo pipefail

release_sha="${RELEASE_SHA:-${GITHUB_SHA:-}}"
current_main_sha="${CURRENT_MAIN_SHA:-}"
github_output="${GITHUB_OUTPUT:-}"
chart_file="charts/shiftpv/Chart.yaml"

if [[ -z "${release_sha}" ]]; then
  echo "RELEASE_SHA or GITHUB_SHA must be set" >&2
  exit 1
fi
if [[ -z "${current_main_sha}" ]]; then
  echo "CURRENT_MAIN_SHA must be set" >&2
  exit 1
fi
if [[ -z "${github_output}" ]]; then
  echo "GITHUB_OUTPUT must be set" >&2
  exit 1
fi

if [[ "${release_sha}" != "${current_main_sha}" ]]; then
  echo "::notice::CI completed for stale main commit ${release_sha}; current main is ${current_main_sha}"
  echo 'should_release=false' >>"${github_output}"
  exit 0
fi

version="$(awk '$1 == "version:" {print $2; exit}' "${chart_file}")"
if ! [[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::${chart_file} version must use numeric major.minor.patch format" >&2
  exit 1
fi

tag="chart/v${version}"
if tagged_commit="$(git rev-parse --verify "refs/tags/${tag}^{commit}" 2>/dev/null)"; then
  if [[ "${tagged_commit}" != "${release_sha}" ]]; then
    echo "::error::tag ${tag} already points to a different commit; bump the chart version before merging" >&2
    exit 1
  fi
  echo "::notice::tag ${tag} already points to this commit; resuming publication"
else
  echo "::notice::tag ${tag} does not exist; publishing the chart"
fi

{
  echo 'should_release=true'
  echo "version=${version}"
  echo "tag=${tag}"
} >>"${github_output}"
