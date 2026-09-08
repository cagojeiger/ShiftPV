#!/usr/bin/env bash
set -euo pipefail

# This suite inherits the isolated runner's KUBECONFIG.
: "${KUBECONFIG:?isolated KUBECONFIG is required}"

query() {
  local expression=$1
  kubectl -n shiftpv-system exec deployment/metrics-test -- \
    promtool query instant http://localhost:9090 "${expression}"
}

wait_query() {
  local expression=$1 result
  for _ in {1..60}; do
    result=$(query "${expression}")
    if [[ -n "${result}" ]]; then
      printf '%s\n' "${expression}: ${result}"
      return
    fi
    sleep 2
  done
  echo "Prometheus assertion failed: ${expression}" >&2
  exit 1
}

wait_query 'count(up{job="shiftpv"} == 1) == 3'
wait_query 'count(up{shiftpv="true",namespace="shiftpv-system"} == 1) == 3'
wait_query 'count(up{shiftpv="true",cluster=~".*",namespace="shiftpv-system"} == 1) == 3'
wait_query 'absent(time() - (vector(0) > 0)) == 1'
wait_query '(time() - (vector(time() - 30) > 0)) == 30'
wait_query 'count(shiftpv_pool_filesystem_available_bytes > 0) == 2'
wait_query 'count(shiftpv_pool_ready == 1) == 2'
wait_query 'count(shiftpv_pool_accounting_valid == 1) == 2'
wait_query 'sum(shiftpv_pool_reserved_bytes) == 0'
wait_query 'sum(shiftpv_moves) == 0'
wait_query 'sum(shiftpv_csi_requests_total{method="CreateVolume",code="OK"}) >= 2'
wait_query 'sum(shiftpv_csi_requests_total{method="DeleteVolume",code="OK"}) >= 2'
wait_query 'sum(shiftpv_csi_requests_total{method="NodePublishVolume",code="OK"}) >= 3'
wait_query 'sum(max_over_time(shiftpv_moves{phase="Copying"}[15m])) > 0'
wait_query 'sum(max_over_time(shiftpv_pool_reserved_bytes[15m])) > 0'

for component in controller node; do
  kubectl get --raw "/api/v1/namespaces/shiftpv-system/services/shiftpv-${component}-metrics:8080/proxy/metrics" |
    kubectl -n shiftpv-system exec -i deployment/metrics-test -- promtool check metrics
done

dashboard="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)/charts/shiftpv/dashboards/shiftpv-overview.json"
expressions=$(jq -er '.panels[].targets[].expr
  | gsub("\\$\\{cluster:regex\\}"; ".*")
  | gsub("\\$\\{namespace:regex\\}"; "shiftpv-system")
  | gsub("\\$\\{pool:regex\\}"; ".*")
  | gsub("\\$__rate_interval"; "5m")' "${dashboard}")
while IFS= read -r expression; do
  query "${expression}" >/dev/null
done <<< "${expressions}"

echo "ShiftPV metrics passed: 3 live targets, Pool capacity, CSI calls, observed copy and zero active moves/reservations after cleanup"
