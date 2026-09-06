#!/usr/bin/env bash
set -euo pipefail

EVIDENCE_DIR=${SHIFTPV_PERF_EVIDENCE_DIR:-}
REJECTED_EXPERIMENT_ACK=${SHIFTPV_REJECTED_NODE_PUBLISH_WAIT_EXPERIMENT:-0}
MAX_PUBLISH_WAIT_MS=${MAX_PUBLISH_WAIT_MS:-5000}
MAX_DOWNTIME_MS=${MAX_DOWNTIME_MS:-45000}
EXPECTED_DATASET_BYTES=${EXPECTED_DATASET_BYTES:-1114701836}

if [[ "${REJECTED_EXPERIMENT_ACK}" != "1" ]]; then
  cat >&2 <<'EOF'
This evaluator belongs to the rejected NodePublish-wait experiment.
It is not a release or readiness gate for the current reservation-Pod design.
Set SHIFTPV_REJECTED_NODE_PUBLISH_WAIT_EXPERIMENT=1 only to re-evaluate that historical evidence.
EOF
  exit 2
fi

if [[ -z "${EVIDENCE_DIR}" ]]; then
  echo "SHIFTPV_PERF_EVIDENCE_DIR is required" >&2
  exit 1
fi

RESULT=${EVIDENCE_DIR}/validation.json
if [[ ! -f "${RESULT}" ]]; then
  echo "performance validation evidence not found: ${RESULT}" >&2
  exit 1
fi

command -v jq >/dev/null || {
  echo "required command not found: jq" >&2
  exit 1
}

jq -e \
  --argjson expectedBytes "${EXPECTED_DATASET_BYTES}" \
  --argjson maxPublishWait "${MAX_PUBLISH_WAIT_MS}" \
  --argjson maxDowntime "${MAX_DOWNTIME_MS}" \
  '
    .dataset_bytes == $expectedBytes and
    .checksum_before != "" and
    .checksum_after == .checksum_before and
    .move_phase == "Succeeded" and
    .volume_phase == "Ready" and
    .destination_node != "" and
    .owner_node == .destination_node and
    .published_nodes == [.destination_node] and
    .active_move == "" and
    .source_final_absent == true and
    .destination_final_present == true and
    .non_test_diff_empty == true and
    .cleanup_complete == true and
    .waiting_for_destination_publish_ms <= $maxPublishWait and
    .downtime_ms <= $maxDowntime
  ' "${RESULT}" >/dev/null

printf 'Rejected NodePublish-wait experiment evidence VALID: publish_wait_ms=%s downtime_ms=%s bytes=%s\n' \
  "$(jq -r '.waiting_for_destination_publish_ms' "${RESULT}")" \
  "$(jq -r '.downtime_ms' "${RESULT}")" \
  "$(jq -r '.dataset_bytes' "${RESULT}")"
