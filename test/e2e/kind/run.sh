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
export KUBECONFIG="${E2E_KUBECONFIG:-${WORK_DIR}/kubeconfig}"

# Avoid inheriting a desktop-specific credential helper when using an isolated
# Docker engine such as Colima. DOCKER_HOST preserves the engine selected before
# DOCKER_CONFIG is switched to the empty per-run directory.
export DOCKER_HOST=${DOCKER_HOST:-${ACTIVE_DOCKER_HOST}}
export DOCKER_CONFIG=${DOCKER_CONFIG_DIR}
unset DOCKER_CONTEXT
docker info >/dev/null

cleanup() {
  if [[ "${KEEP_CLUSTER}" == "1" ]]; then
    echo "keeping cluster ${CLUSTER_NAME}, kubeconfig ${KUBECONFIG}, and data under ${WORK_DIR}"
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
sed \
  -e "s|__WORKER_A_NODE__|${CLUSTER_NAME}-worker|g" \
  -e "s|__WORKER_B_NODE__|${CLUSTER_NAME}-worker2|g" \
  "${ROOT_DIR}/test/e2e/kind/pools.yaml.tpl" > "${WORK_DIR}/pools.yaml"

kind create cluster \
  --name "${CLUSTER_NAME}" \
  --image "${NODE_IMAGE}" \
  --config "${WORK_DIR}/cluster.yaml"

docker build \
  --target combined \
  --build-arg CONTROLLER_VERSION=dev \
  --build-arg NODE_VERSION=dev \
  -f "${ROOT_DIR}/build/package/Dockerfile" \
  -t shiftpv:dev \
  "${ROOT_DIR}"
kind load docker-image shiftpv:dev --name "${CLUSTER_NAME}"

install_shiftpv() {
	local default_class=${1:-true}
	helm upgrade --install shiftpv "${ROOT_DIR}/charts/shiftpv" \
		--namespace shiftpv-system \
		--create-namespace \
		--values "${ROOT_DIR}/test/e2e/kind/values.yaml" \
		--set "storageClass.defaultClass=${default_class}" \
		--wait \
		--timeout 5m
	kubectl -n shiftpv-system wait \
    --for=condition=Ready pod \
    -l app.kubernetes.io/instance=shiftpv \
		--timeout=5m
	kubectl apply -f "${WORK_DIR}/pools.yaml"
}

run_mobility_filesystem_faults() {
	CLUSTER_NAME="${CLUSTER_NAME}" WORK_DIR="${WORK_DIR}" \
		WORKER_A_POOL="${WORKER_A_POOL}" WORKER_B_POOL="${WORKER_B_POOL}" \
		"${ROOT_DIR}/test/e2e/kind/mobility-filesystem-faults.sh"
}

install_shiftpv true

if [[ "${MOBILITY_FILESYSTEM_FAULTS_ONLY:-0}" == "1" ]]; then
	run_mobility_filesystem_faults
	echo "ShiftPV focused mobility filesystem fault E2E passed"
	exit 0
fi

# Lifecycle admission is read-only. A direct dry-run DELETE must not mint an
# uninstall permit even when no storage dependency exists.
if kubectl -n shiftpv-system delete deployment shiftpv-controller --dry-run=server; then
	echo "direct dry-run DELETE unexpectedly bypassed the uninstall guard" >&2
	exit 1
fi
kubectl -n shiftpv-system get deployment/shiftpv-controller >/dev/null
if kubectl -n shiftpv-system get configmap/shiftpv-uninstall-permit >/dev/null 2>&1; then
	echo "direct dry-run DELETE created uninstall state" >&2
	exit 1
fi

# Pool registration alone is safe to retain. With no PVC/PV/Volume/Move, the
# hook must allow a normal uninstall and delete its successful Job.
helm uninstall shiftpv --namespace shiftpv-system --timeout 2m
if kubectl -n shiftpv-system get job shiftpv-uninstall-guard >/dev/null 2>&1; then
  echo "successful uninstall guard Job was not deleted" >&2
  exit 1
fi
install_shiftpv true

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

# Force replacement of both controller and owner-node plugin Pods while the
# workload keeps the volume mounted. Their Kubernetes UIDs must change.
CONTROLLER_POD_BEFORE=$(kubectl -n shiftpv-system get pod \
  -l app.kubernetes.io/instance=shiftpv,app.kubernetes.io/component=controller \
  -o jsonpath='{.items[0].metadata.name}')
CONTROLLER_UID_BEFORE=$(kubectl -n shiftpv-system get "pod/${CONTROLLER_POD_BEFORE}" \
  -o jsonpath='{.metadata.uid}')
kubectl -n shiftpv-system delete "pod/${CONTROLLER_POD_BEFORE}" \
  --grace-period=0 --force --wait=true
kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=5m
CONTROLLER_UID_AFTER=$(kubectl -n shiftpv-system get pod \
  -l app.kubernetes.io/instance=shiftpv,app.kubernetes.io/component=controller \
  -o jsonpath='{.items[0].metadata.uid}')
if [[ "${CONTROLLER_UID_BEFORE}" == "${CONTROLLER_UID_AFTER}" ]]; then
  echo "controller Pod UID did not change after forced replacement" >&2
  exit 1
fi

NODE_POD_BEFORE=$(kubectl -n shiftpv-system get pod \
  -l app.kubernetes.io/instance=shiftpv,app.kubernetes.io/component=node \
  --field-selector "spec.nodeName=${OWNER_NODE}" \
  -o jsonpath='{.items[0].metadata.name}')
NODE_UID_BEFORE=$(kubectl -n shiftpv-system get "pod/${NODE_POD_BEFORE}" \
  -o jsonpath='{.metadata.uid}')
kubectl -n shiftpv-system delete "pod/${NODE_POD_BEFORE}" \
  --grace-period=0 --force --wait=true
kubectl -n shiftpv-system rollout status daemonset/shiftpv-node --timeout=5m
NODE_UID_AFTER=$(kubectl -n shiftpv-system get pod \
  -l app.kubernetes.io/instance=shiftpv,app.kubernetes.io/component=node \
  --field-selector "spec.nodeName=${OWNER_NODE}" \
  -o jsonpath='{.items[0].metadata.uid}')
if [[ "${NODE_UID_BEFORE}" == "${NODE_UID_AFTER}" ]]; then
  echo "owner-node plugin Pod UID did not change after forced replacement" >&2
  exit 1
fi

kubectl wait --for=condition=Ready pod/shiftpv-e2e --timeout=2m
CHECKSUM_AFTER_RESTARTS=$(kubectl exec shiftpv-e2e -- sha256sum /data/payload | awk '{print $1}')
if [[ "${CHECKSUM_BEFORE}" != "${CHECKSUM_AFTER_RESTARTS}" ]]; then
  echo "checksum mismatch after controller and node plugin replacement" >&2
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

# A failed pre-delete hook must leave the release and the running workload intact.
if helm uninstall shiftpv --namespace shiftpv-system --timeout 2m; then
  echo "Helm uninstall unexpectedly succeeded while a ShiftPV workload was running" >&2
  exit 1
fi
helm status shiftpv --namespace shiftpv-system >/dev/null
kubectl -n shiftpv-system get deployment/shiftpv-controller >/dev/null
kubectl -n shiftpv-system get daemonset/shiftpv-node >/dev/null
kubectl get validatingwebhookconfiguration shiftpv-lifecycle >/dev/null
kubectl -n shiftpv-system wait --for=delete configmap/shiftpv-uninstall-permit --timeout=30s
kubectl -n shiftpv-system logs job/shiftpv-uninstall-guard | grep -F 'ShiftPV uninstall denied'
CHECKSUM_AFTER_DENIAL=$(kubectl exec shiftpv-e2e -- sha256sum /data/payload | awk '{print $1}')
if [[ "${CHECKSUM_BEFORE}" != "${CHECKSUM_AFTER_DENIAL}" ]]; then
  echo "checksum mismatch after denied Helm uninstall" >&2
  exit 1
fi

# Stopping the Pod is not enough: retained PVC/PV/Volume state still requires an
# explicit recovery decision. A second denial also exercises hook replacement.
kubectl delete pod shiftpv-e2e --wait=true
if helm uninstall shiftpv --namespace shiftpv-system --timeout 2m; then
  echo "Helm uninstall unexpectedly succeeded while retained ShiftPV resources existed" >&2
  exit 1
fi
helm status shiftpv --namespace shiftpv-system >/dev/null
kubectl -n shiftpv-system logs job/shiftpv-uninstall-guard | grep -F 'PersistentVolume'

# The break-glass path deliberately bypasses hooks. Remove the failed hook Job,
# which Helm does not own, so the subsequent reinstall starts without leftovers.
kubectl -n shiftpv-system delete job shiftpv-uninstall-guard --ignore-not-found --wait=true
kubectl delete validatingwebhookconfiguration shiftpv-lifecycle --ignore-not-found --wait=true
helm uninstall shiftpv --namespace shiftpv-system --no-hooks

kubectl get pvc shiftpv-e2e >/dev/null
kubectl get "pv/${PV_NAME}" >/dev/null
kubectl -n shiftpv-system get "configmap/${VOLUME_ID}" >/dev/null

case "${OWNER_NODE}" in
  "${CLUSTER_NAME}-worker") DATA_ROOT=${WORKER_A_POOL} ;;
  "${CLUSTER_NAME}-worker2") DATA_ROOT=${WORKER_B_POOL} ;;
  *) echo "unexpected owner node: ${OWNER_NODE}" >&2; exit 1 ;;
esac
test -f "${DATA_ROOT}/volumes/${VOLUME_ID}/payload"

install_shiftpv true
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/pod.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-e2e --timeout=5m
CHECKSUM_AFTER=$(kubectl exec shiftpv-e2e -- sha256sum /data/payload | awk '{print $1}')

if [[ "${CHECKSUM_BEFORE}" != "${CHECKSUM_AFTER}" ]]; then
	echo "checksum mismatch after Helm reinstall" >&2
	exit 1
fi

# Reinstall with ShiftPV opt-in while an unrelated default class already exists.
kubectl delete pod shiftpv-e2e --wait=true
kubectl delete validatingwebhookconfiguration shiftpv-lifecycle --ignore-not-found --wait=true
helm uninstall shiftpv --namespace shiftpv-system --no-hooks
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/existing-default-storageclass.yaml"
install_shiftpv false

EXISTING_DEFAULT=$(kubectl get storageclass existing-default \
	-o jsonpath='{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}')
SHIFTPV_DEFAULT=$(kubectl get storageclass shiftpv \
	-o jsonpath='{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}')
if [[ "${EXISTING_DEFAULT}" != "true" || "${SHIFTPV_DEFAULT}" != "false" ]]; then
	echo "StorageClass default annotations changed unexpectedly: existing=${EXISTING_DEFAULT} shiftpv=${SHIFTPV_DEFAULT}" >&2
	exit 1
fi

kubectl apply -f "${ROOT_DIR}/test/e2e/kind/implicit-pvc.yaml"
kubectl wait \
	--for=jsonpath='{.spec.storageClassName}'=existing-default \
	pvc/existing-default-e2e \
	--timeout=2m

kubectl apply -f "${ROOT_DIR}/test/e2e/kind/coexistence-pvc.yaml"
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/coexistence-pod.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-coexistence --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/shiftpv-coexistence --timeout=2m
COEXISTENCE_PV=$(kubectl get pvc shiftpv-coexistence -o jsonpath='{.spec.volumeName}')
COEXISTENCE_DRIVER=$(kubectl get "pv/${COEXISTENCE_PV}" -o jsonpath='{.spec.csi.driver}')
if [[ "${COEXISTENCE_DRIVER}" != "csi.shiftpv.io" ]]; then
	echo "explicit ShiftPV PVC used unexpected driver: ${COEXISTENCE_DRIVER}" >&2
	exit 1
fi
kubectl exec shiftpv-coexistence -- grep -Fx 'ShiftPV StorageClass coexistence' /data/payload

kubectl delete pod shiftpv-coexistence --wait=true
kubectl delete pvc shiftpv-coexistence --wait=true
CLUSTER_NAME="${CLUSTER_NAME}" WORKER_B_POOL="${WORKER_B_POOL}" \
  "${ROOT_DIR}/test/e2e/kind/filesystem-faults.sh"
run_mobility_filesystem_faults

echo "ShiftPV kind e2e passed"
echo "PV=${PV_NAME} volume=${VOLUME_ID} node=${OWNER_NODE} checksum=${CHECKSUM_AFTER} controller_restart=${CONTROLLER_UID_AFTER} node_restart=${NODE_UID_AFTER}"
