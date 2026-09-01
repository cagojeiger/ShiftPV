#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
CLUSTER_NAME=${CLUSTER_NAME:-shiftpv-e2e}
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0}
KEEP_CLUSTER=${KEEP_CLUSTER:-0}

for command in docker kind kubectl helm sed; do
  command -v "${command}" >/dev/null || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done

KIND_VERSION=$(kind version | awk '{print $2}' | sed 's/^v//')
IFS=. read -r KIND_MAJOR KIND_MINOR _ <<<"${KIND_VERSION}"
if ((10#${KIND_MAJOR} == 0 && 10#${KIND_MINOR} < 33)); then
  echo "kind >= 0.33.0 is required for the pinned Kubernetes 1.35.8 node image; found ${KIND_VERSION}" >&2
  exit 1
fi

ACTIVE_DOCKER_CONTEXT=$(docker context show)
ACTIVE_DOCKER_HOST=$(docker context inspect "${ACTIVE_DOCKER_CONTEXT}" --format '{{.Endpoints.docker.Host}}')

mkdir -p "${ROOT_DIR}/.tmp"
WORK_DIR=$(mktemp -d "${ROOT_DIR}/.tmp/shiftpv-e2e.XXXXXX")
WORKER_A_POOL="${WORK_DIR}/worker-a"
WORKER_B_POOL="${WORK_DIR}/worker-b"
DOCKER_CONFIG_DIR="${WORK_DIR}/docker-config"
mkdir -p "${WORKER_A_POOL}" "${WORKER_B_POOL}" "${DOCKER_CONFIG_DIR}"

# Avoid inheriting a desktop-specific credential helper when using an isolated
# Docker engine such as Colima. DOCKER_HOST preserves the engine selected before
# DOCKER_CONFIG is switched to the empty per-run directory.
export DOCKER_HOST=${DOCKER_HOST:-${ACTIVE_DOCKER_HOST}}
export DOCKER_CONFIG=${DOCKER_CONFIG_DIR}
unset DOCKER_CONTEXT
docker info >/dev/null

cleanup() {
  if [[ "${KEEP_CLUSTER}" == "1" ]]; then
    echo "keeping cluster ${CLUSTER_NAME} and data under ${WORK_DIR}"
    return
  fi
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  rm -rf -- "${WORK_DIR}"
}
trap cleanup EXIT

sed \
  -e "s|__WORKER_A_POOL__|${WORKER_A_POOL}|g" \
  -e "s|__WORKER_B_POOL__|${WORKER_B_POOL}|g" \
  "${ROOT_DIR}/test/e2e/kind/cluster.yaml.tpl" > "${WORK_DIR}/cluster.yaml"

kind create cluster \
  --name "${CLUSTER_NAME}" \
  --image "${NODE_IMAGE}" \
  --config "${WORK_DIR}/cluster.yaml"

docker build \
  --build-arg VERSION=dev \
  -f "${ROOT_DIR}/build/package/Dockerfile" \
  -t shiftpv:dev \
  "${ROOT_DIR}"
kind load docker-image shiftpv:dev --name "${CLUSTER_NAME}"

install_shiftpv() {
  helm upgrade --install shiftpv "${ROOT_DIR}/charts/shiftpv" \
    --namespace shiftpv-system \
    --create-namespace \
    --values "${ROOT_DIR}/test/e2e/kind/values.yaml" \
    --wait \
    --timeout 5m
  kubectl -n shiftpv-system wait \
    --for=condition=Ready pod \
    -l app.kubernetes.io/instance=shiftpv \
    --timeout=5m
}

install_shiftpv
DEFAULT_CLASS=$(kubectl get storageclass shiftpv \
  -o jsonpath='{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}')
if [[ "${DEFAULT_CLASS}" != "true" ]]; then
  echo "shiftpv StorageClass is not marked as the cluster default" >&2
  exit 1
fi

kubectl apply -f "${ROOT_DIR}/test/e2e/kind/pvc.yaml"
kubectl wait \
  --for=jsonpath='{.spec.storageClassName}'=shiftpv \
  pvc/shiftpv-e2e \
  --timeout=2m
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/pod.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-e2e --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/shiftpv-e2e --timeout=2m

PV_NAME=$(kubectl get pvc shiftpv-e2e -o jsonpath='{.spec.volumeName}')
OWNER_NODE=$(kubectl get pod shiftpv-e2e -o jsonpath='{.spec.nodeName}')
CHECKSUM_BEFORE=$(kubectl exec shiftpv-e2e -- sha256sum /data/payload | awk '{print $1}')
VOLUME_ID=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
PV_DRIVER=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.driver}')
if [[ "${PV_DRIVER}" != "csi.shiftpv.io" ]]; then
  echo "PVC was not provisioned by ShiftPV: ${PV_DRIVER}" >&2
  exit 1
fi

# Verify ordinary kubelet unpublish/publish before testing the Helm boundary.
kubectl delete pod shiftpv-e2e --wait=true
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/pod.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-e2e --timeout=5m
CHECKSUM_RECREATED=$(kubectl exec shiftpv-e2e -- sha256sum /data/payload | awk '{print $1}')
if [[ "${CHECKSUM_BEFORE}" != "${CHECKSUM_RECREATED}" ]]; then
  echo "checksum mismatch after Pod recreation" >&2
  exit 1
fi

kubectl delete pod shiftpv-e2e --wait=true
helm uninstall shiftpv --namespace shiftpv-system

kubectl get pvc shiftpv-e2e >/dev/null
kubectl get "pv/${PV_NAME}" >/dev/null
kubectl -n shiftpv-system get "configmap/${VOLUME_ID}" >/dev/null

case "${OWNER_NODE}" in
  "${CLUSTER_NAME}-worker") DATA_ROOT=${WORKER_A_POOL} ;;
  "${CLUSTER_NAME}-worker2") DATA_ROOT=${WORKER_B_POOL} ;;
  *) echo "unexpected owner node: ${OWNER_NODE}" >&2; exit 1 ;;
esac
test -f "${DATA_ROOT}/volumes/${VOLUME_ID}/payload"

install_shiftpv
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/pod.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-e2e --timeout=5m
CHECKSUM_AFTER=$(kubectl exec shiftpv-e2e -- sha256sum /data/payload | awk '{print $1}')

if [[ "${CHECKSUM_BEFORE}" != "${CHECKSUM_AFTER}" ]]; then
  echo "checksum mismatch after Helm reinstall" >&2
  exit 1
fi

echo "ShiftPV kind e2e passed"
echo "PV=${PV_NAME} volume=${VOLUME_ID} node=${OWNER_NODE} checksum=${CHECKSUM_AFTER}"
