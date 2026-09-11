#!/usr/bin/env bash
set -euo pipefail

release_sha="${RELEASE_SHA:-${GITHUB_SHA:-}}"
current_main_sha="${CURRENT_MAIN_SHA:-}"
github_output="${GITHUB_OUTPUT:-}"

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

components='[]'
builds='[]'

if ! git merge-base --is-ancestor "${release_sha}" "${current_main_sha}" 2>/dev/null; then
  echo "::notice::release commit ${release_sha} is not in current main history ${current_main_sha}"
  {
    echo 'should_release=false'
    echo 'components=[]'
    echo 'builds=[]'
  } >>"${github_output}"
  exit 0
fi
if [[ "${release_sha}" != "${current_main_sha}" ]]; then
  echo "::notice::main advanced to ${current_main_sha}; reconciling release commit ${release_sha}"
fi

for component in controller node; do
  version_file="versions/${component}"
  version="$(sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' "${version_file}")"
  if ! [[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "::error::${version_file} must use numeric major.minor.patch format"
    exit 1
  fi

  tag="${component}/v${version}"
  version_changed=false
  if git rev-parse "${release_sha}^1" >/dev/null 2>&1; then
    if ! git diff --quiet "${release_sha}^1" "${release_sha}" -- "${version_file}"; then
      version_changed=true
    fi
  else
    version_changed=true
  fi

  if [[ "${version_changed}" != true ]]; then
    echo "::notice::${version_file} did not change in ${release_sha}; skipping ${component}"
    continue
  fi

  if ! desired_version="$(git show "${current_main_sha}:${version_file}" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"; then
    echo "::error::cannot read ${version_file} from current main ${current_main_sha}"
    exit 1
  fi
  if ! [[ "${desired_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "::error::current main ${version_file} must use numeric major.minor.patch format"
    exit 1
  fi
  if [[ "${version}" != "${desired_version}" ]]; then
    echo "::notice::${component} v${version} was superseded by current main v${desired_version}; skipping"
    continue
  fi

  if tagged_commit="$(git rev-parse --verify "refs/tags/${tag}^{commit}" 2>/dev/null)"; then
    if [[ "${tagged_commit}" == "${release_sha}" ]]; then
      echo "::notice::tag ${tag} already points to this commit; resuming release"
    else
      echo "::error::tag ${tag} already points to a different commit; bump ${version_file} before merging"
      exit 1
    fi
  fi

  version_arg="${component^^}_VERSION"
  descriptor="$(jq -cn \
    --arg component "${component}" \
    --arg version "${version}" \
    --arg version_arg "${version_arg}" \
    --arg tag "${tag}" \
    '{component: $component, version: $version, version_arg: $version_arg, tag: $tag}')"
  components="$(jq -cn --argjson items "${components}" --argjson item "${descriptor}" '$items + [$item]')"

  for platform_spec in 'linux/amd64|ubuntu-latest|amd64' 'linux/arm64|ubuntu-24.04-arm|arm64'; do
    IFS='|' read -r platform runner arch <<<"${platform_spec}"
    build="$(jq -cn \
      --arg component "${component}" \
      --arg version "${version}" \
      --arg version_arg "${version_arg}" \
      --arg platform "${platform}" \
      --arg runner "${runner}" \
      --arg arch "${arch}" \
      '{component: $component, version: $version, version_arg: $version_arg, platform: $platform, runner: $runner, arch: $arch}')"
    builds="$(jq -cn --argjson items "${builds}" --argjson item "${build}" '$items + [$item]')"
  done
done

should_release=false
if [[ "$(jq 'length' <<<"${components}")" -gt 0 ]]; then
  should_release=true
fi

{
  echo "should_release=${should_release}"
  echo "components=${components}"
  echo "builds=${builds}"
} >>"${github_output}"
