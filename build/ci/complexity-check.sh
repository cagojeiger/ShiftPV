#!/usr/bin/env bash
# Keep production functions within the cyclomatic complexity ceiling established
# by the 0.4 refactor. Tests are excluded because table-driven assertions and
# fixture construction do not carry the same maintenance risk as runtime code.
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
GOCYCLO_VERSION=v0.6.0

output=$(cd "${ROOT_DIR}" && go run "github.com/fzipp/gocyclo/cmd/gocyclo@${GOCYCLO_VERSION}" \
	-over 20 -ignore '_test\.go$' src)

if [[ -n ${output} ]]; then
	printf '%s\n' "${output}" >&2
	printf 'cyclomatic complexity exceeds 20\n' >&2
	exit 1
fi

printf 'all production functions have cyclomatic complexity <= 20\n'
