#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
image_workflow="${repo_root}/.github/workflows/release-images.yaml"
chart_workflow="${repo_root}/.github/workflows/release-chart.yaml"

grep -Fq 'workflows: [CI]' "${image_workflow}"
grep -Fq 'workflows: [Release Images]' "${chart_workflow}"
grep -Fq "github.event.workflow_run.event == 'workflow_run'" "${chart_workflow}"
grep -Fq 'run: build/ci/wait-for-chart-images.sh' "${chart_workflow}"

echo "release workflow order validation passed"
