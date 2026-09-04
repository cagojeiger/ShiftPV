#!/usr/bin/env bash
set -euo pipefail

TEST_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT_DIR=$(cd "${TEST_DIR}/../../../.." && pwd)
LOCK_FILE=${ARTIFACT_LOCK_FILE:-"${TEST_DIR}/versions.env"}
CLUSTER_NAME=${CLUSTER_NAME:-shiftpv-artifact-e2e}
KEEP_CLUSTER=${KEEP_CLUSTER:-0}

for command in awk docker helm kind kubectl sed; do
  command -v "${command}" >/dev/null || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done
"${TEST_DIR}/validate-lock.sh" "${LOCK_FILE}"
# The validator restricts values to non-executable URL/version/digest forms.
# shellcheck disable=SC1090
source "${LOCK_FILE}"

if command -v sha256sum >/dev/null 2>&1; then
  sha256_file() { sha256sum "$1" | awk '{ print $1 }'; }
else
  sha256_file() { shasum -a 256 "$1" | awk '{ print $1 }'; }
fi

kind_version=$(kind version | awk '{ print $2 }' | sed 's/^v//')
IFS=. read -r kind_major kind_minor _ <<<"${kind_version}"
if ((10#${kind_major} == 0 && 10#${kind_minor} < 33)); then
  echo "kind >= 0.33.0 is required; found ${kind_version}" >&2
  exit 1
fi

active_docker_context=$(docker context show)
active_docker_host=$(docker context inspect "${active_docker_context}" --format '{{.Endpoints.docker.Host}}')
mkdir -p "${ROOT_DIR}/.tmp"
work_dir=$(mktemp -d "${ROOT_DIR}/.tmp/shiftpv-artifact.XXXXXX")
worker_a_pool="${work_dir}/worker-a"
worker_b_pool="${work_dir}/worker-b"
mkdir -p "${worker_a_pool}" "${worker_b_pool}" "${work_dir}/docker-config" \
  "${work_dir}/helm-config" "${work_dir}/helm-cache" "${work_dir}/helm-data"
export KUBECONFIG="${E2E_KUBECONFIG:-${work_dir}/kubeconfig}"
export DOCKER_HOST=${DOCKER_HOST:-${active_docker_host}}
export DOCKER_CONFIG="${work_dir}/docker-config"
export HELM_CONFIG_HOME="${work_dir}/helm-config"
export HELM_CACHE_HOME="${work_dir}/helm-cache"
export HELM_DATA_HOME="${work_dir}/helm-data"
unset DOCKER_CONTEXT

cleanup() {
  if [[ "${KEEP_CLUSTER}" == 1 ]]; then
    echo "keeping cluster ${CLUSTER_NAME}, kubeconfig ${KUBECONFIG}, and data under ${work_dir}"
    return
  fi
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  rm -rf -- "${work_dir}"
}
trap cleanup EXIT

sed -e "s|__WORKER_A_POOL__|${worker_a_pool}|g" \
  -e "s|__WORKER_B_POOL__|${worker_b_pool}|g" \
  "${ROOT_DIR}/test/e2e/kind/cluster.yaml.tpl" >"${work_dir}/cluster.yaml"
sed -e "s|__WORKER_A_NODE__|${CLUSTER_NAME}-worker|g" \
  -e "s|__WORKER_B_NODE__|${CLUSTER_NAME}-worker2|g" \
  "${ROOT_DIR}/test/e2e/kind/pools.yaml.tpl" >"${work_dir}/pools.yaml"

helm repo add shiftpv-public "${CHART_REPOSITORY}"
helm repo update shiftpv-public
helm pull shiftpv-public/shiftpv --version "${CHART_VERSION}" --destination "${work_dir}"
chart_package="${work_dir}/shiftpv-${CHART_VERSION}.tgz"
actual_chart_sha=$(sha256_file "${chart_package}")
if [[ "${actual_chart_sha}" != "${CHART_SHA256}" ]]; then
  echo "public chart digest mismatch: expected ${CHART_SHA256}, got ${actual_chart_sha}" >&2
  exit 1
fi
test "$(helm show chart "${chart_package}" | awk '$1 == "version:" { print $2; exit }')" = "${CHART_VERSION}"

kind create cluster --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" \
  --config "${work_dir}/cluster.yaml"

controller_repository=${CONTROLLER_IMAGE%%:*}
controller_tag=${CONTROLLER_IMAGE#*:}
node_repository=${NODE_IMAGE%%:*}
node_tag=${NODE_IMAGE#*:}
helm upgrade --install shiftpv "${chart_package}" \
  --namespace shiftpv-system --create-namespace \
  --set storageClass.defaultClass=true \
  --set-string controller.image.repository="${controller_repository}" \
  --set-string controller.image.tag="${controller_tag}" \
  --set-string node.image.repository="${node_repository}" \
  --set-string node.image.tag="${node_tag}" \
  --set-string mobility.helperImage="${CONTROLLER_IMAGE}" \
  --wait --timeout 8m
kubectl -n shiftpv-system wait --for=condition=Ready pod \
  -l app.kubernetes.io/instance=shiftpv --timeout=5m
kubectl apply -f "${work_dir}/pools.yaml"

test "$(helm -n shiftpv-system list -o json | awk -v version="shiftpv-${CHART_VERSION}" '
  index($0, "\"chart\":\"" version "\"") { print version }')" = "shiftpv-${CHART_VERSION}"
test "$(kubectl -n shiftpv-system get deployment/shiftpv-controller \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="shiftpv-controller")].image}')" = "${CONTROLLER_IMAGE}"
actual_node_image=$(kubectl -n shiftpv-system get daemonset/shiftpv-node \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="shiftpv-node")].image}')
test "${actual_node_image}" = "${NODE_IMAGE}"

kubectl apply -f "${ROOT_DIR}/test/e2e/kind/pvc.yaml"
kubectl wait --for=jsonpath='{.spec.storageClassName}'=shiftpv pvc/shiftpv-e2e --timeout=2m
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/pod.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-e2e --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/shiftpv-e2e --timeout=2m
pv_name=$(kubectl get pvc/shiftpv-e2e -o jsonpath='{.spec.volumeName}')
volume_id=$(kubectl get "pv/${pv_name}" -o jsonpath='{.spec.csi.volumeHandle}')
test "$(kubectl get "pv/${pv_name}" -o jsonpath='{.spec.csi.driver}')" = csi.shiftpv.io
test "$(kubectl get "shiftpvvolume/${volume_id}" -o jsonpath='{.status.phase}')" = Ready
owner_node=$(kubectl get "shiftpvvolume/${volume_id}" -o jsonpath='{.status.ownerNode}')
test "$(kubectl get pod/shiftpv-e2e -o jsonpath='{.spec.nodeName}')" = "${owner_node}"
checksum=$(kubectl exec pod/shiftpv-e2e -- sha256sum /data/payload | awk '{ print $1 }')
test -n "${checksum}"

echo "ShiftPV public artifact smoke passed: chart=${CHART_VERSION} chartSHA256=${CHART_SHA256} controller=${CONTROLLER_IMAGE} node=${NODE_IMAGE} volume=${volume_id} owner=${owner_node} checksum=${checksum}"
