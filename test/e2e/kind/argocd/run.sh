#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)
CLUSTER_NAME=${CLUSTER_NAME:-shiftpv-argocd-e2e}
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0}
ARGOCD_VERSION=${ARGOCD_VERSION:-v3.5.2}
ARGOCD_MANIFEST_SHA256=${ARGOCD_MANIFEST_SHA256:-9a87f2b3e14c278f12501eb0ef5c3955b27cf05370ca425381c6a908cf85a5c5}
IMAGE_REPOSITORY=${IMAGE_REPOSITORY:-shiftpv-argocd-e2e}
IMAGE_TAG=${IMAGE_TAG:-dev}
KEEP_CLUSTER=${KEEP_CLUSTER:-0}

for command in awk curl docker helm kind kubectl sed; do
	command -v "${command}" >/dev/null || {
		echo "required command not found: ${command}" >&2
		exit 1
	}
done

sha256_file() {
	if command -v sha256sum >/dev/null; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		echo "neither sha256sum nor shasum is available" >&2
		return 1
	fi
}

mkdir -p "${ROOT_DIR}/.tmp"
WORK_DIR=$(mktemp -d "${ROOT_DIR}/.tmp/shiftpv-argocd.XXXXXX")
WORKER_POOL="${WORK_DIR}/worker"
CHART_REPOSITORY="${WORK_DIR}/chart-repository"
DOCKER_CONFIG_DIR="${WORK_DIR}/docker-config"
mkdir -p "${WORKER_POOL}" "${CHART_REPOSITORY}" "${DOCKER_CONFIG_DIR}"
export KUBECONFIG="${E2E_KUBECONFIG:-${WORK_DIR}/kubeconfig}"

ACTIVE_DOCKER_CONTEXT=$(docker context show)
ACTIVE_DOCKER_HOST=$(docker context inspect "${ACTIVE_DOCKER_CONTEXT}" --format '{{.Endpoints.docker.Host}}')
export DOCKER_HOST=${DOCKER_HOST:-${ACTIVE_DOCKER_HOST}}
export DOCKER_CONFIG=${DOCKER_CONFIG_DIR}
unset DOCKER_CONTEXT

cleanup() {
	if [[ "${KEEP_CLUSTER}" == "1" ]]; then
		echo "keeping cluster ${CLUSTER_NAME}, kubeconfig ${KUBECONFIG}, and data under ${WORK_DIR}"
		return
	fi
	kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
	rm -rf -- "${WORK_DIR}"
}
trap cleanup EXIT

sed "s|__WORKER_POOL__|${WORKER_POOL}|g" \
	"${ROOT_DIR}/test/e2e/kind/argocd/cluster.yaml.tpl" >"${WORK_DIR}/cluster.yaml"
sed "s|__WORKER_NODE__|${CLUSTER_NAME}-worker|g" \
	"${ROOT_DIR}/test/e2e/kind/argocd/pool.yaml.tpl" >"${WORK_DIR}/pool.yaml"

kind create cluster --name "${CLUSTER_NAME}" --image "${NODE_IMAGE}" --config "${WORK_DIR}/cluster.yaml"

docker build \
	--target combined \
	--build-arg CONTROLLER_VERSION=dev \
	--build-arg NODE_VERSION=dev \
	-f "${ROOT_DIR}/build/package/Dockerfile" \
	-t "${IMAGE_REPOSITORY}:${IMAGE_TAG}" \
	"${ROOT_DIR}"
kind load docker-image "${IMAGE_REPOSITORY}:${IMAGE_TAG}" --name "${CLUSTER_NAME}"

CHART_VERSION=$(helm show chart "${ROOT_DIR}/charts/shiftpv" | awk '$1 == "version:" {print $2}')
test -n "${CHART_VERSION}"
helm package "${ROOT_DIR}/charts/shiftpv" --destination "${CHART_REPOSITORY}" >/dev/null
helm repo index "${CHART_REPOSITORY}"

kubectl create namespace shiftpv-chart-repository
kubectl -n shiftpv-chart-repository create configmap shiftpv-chart-repository \
	--from-file="${CHART_REPOSITORY}/index.yaml" \
	--from-file="${CHART_REPOSITORY}/shiftpv-${CHART_VERSION}.tgz"
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/argocd/chart-repository.yaml"
kubectl -n shiftpv-chart-repository rollout status deployment/shiftpv-chart-repository --timeout=3m

ARGOCD_MANIFEST="${WORK_DIR}/argocd-install.yaml"
curl -fsSL "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml" \
	-o "${ARGOCD_MANIFEST}"
test "$(sha256_file "${ARGOCD_MANIFEST}")" = "${ARGOCD_MANIFEST_SHA256}"
kubectl create namespace argocd
kubectl apply --server-side --force-conflicts -n argocd -f "${ARGOCD_MANIFEST}" >/dev/null

for deployment in \
	argocd-applicationset-controller \
	argocd-dex-server \
	argocd-notifications-controller \
	argocd-redis \
	argocd-repo-server \
	argocd-server; do
	kubectl -n argocd rollout status "deployment/${deployment}" --timeout=10m
done
kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=10m

sed \
	-e "s|__CHART_VERSION__|${CHART_VERSION}|g" \
	-e "s|__IMAGE_REPOSITORY__|${IMAGE_REPOSITORY}|g" \
	-e "s|__IMAGE_TAG__|${IMAGE_TAG}|g" \
	"${ROOT_DIR}/test/e2e/kind/argocd/application.yaml.tpl" >"${WORK_DIR}/application.yaml"

apply_and_wait_for_application() {
	kubectl apply -f "${WORK_DIR}/application.yaml"
	kubectl -n argocd wait --for=jsonpath='{.status.sync.status}'=Synced application/shiftpv --timeout=10m
	kubectl -n argocd wait --for=jsonpath='{.status.health.status}'=Healthy application/shiftpv --timeout=10m
	kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=5m
	kubectl -n shiftpv-system rollout status daemonset/shiftpv-node --timeout=5m
}

# No dependent storage exists, so Argo CD must execute the PreDelete hook and
# finish cascading Application deletion.
apply_and_wait_for_application
kubectl -n argocd delete application shiftpv --wait=false
kubectl -n argocd wait --for=delete application/shiftpv --timeout=5m
if kubectl get storageclass shiftpv >/dev/null 2>&1; then
	echo "Argo CD left the ShiftPV StorageClass after an allowed Application deletion" >&2
	exit 1
fi
if kubectl -n shiftpv-system get job shiftpv-uninstall-guard >/dev/null 2>&1; then
	echo "successful Argo CD uninstall guard Job was not deleted" >&2
	exit 1
fi

# Reinstall and create a real mounted volume. The same PreDelete hook must now
# keep the Application and every driver component alive.
apply_and_wait_for_application
kubectl apply -f "${WORK_DIR}/pool.yaml"
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/argocd/workload.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-argocd-e2e --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/shiftpv-argocd-e2e --timeout=2m

PV_NAME=$(kubectl get pvc shiftpv-argocd-e2e -o jsonpath='{.spec.volumeName}')
VOLUME_ID=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
CHECKSUM_BEFORE=$(kubectl exec shiftpv-argocd-e2e -- sha256sum /data/payload | awk '{print $1}')

kubectl -n argocd delete application shiftpv --wait=false
kubectl -n shiftpv-system wait --for=condition=Failed job/shiftpv-uninstall-guard --timeout=5m
test -n "$(kubectl -n argocd get application shiftpv -o jsonpath='{.metadata.deletionTimestamp}')"

DELETION_ERROR=""
for _ in {1..120}; do
	DELETION_ERROR=$(kubectl -n argocd get application shiftpv \
		-o jsonpath='{range .status.conditions[?(@.type=="DeletionError")]}{.message}{end}' 2>/dev/null || true)
	[[ -n "${DELETION_ERROR}" ]] && break
	sleep 1
done
if [[ -z "${DELETION_ERROR}" ]]; then
	echo "Argo CD Application did not report DeletionError after the uninstall guard failed" >&2
	exit 1
fi

kubectl -n shiftpv-system get deployment/shiftpv-controller >/dev/null
kubectl -n shiftpv-system get daemonset/shiftpv-node >/dev/null
kubectl get storageclass shiftpv >/dev/null
kubectl get validatingwebhookconfiguration shiftpv-lifecycle >/dev/null

# Argo CD keeps retrying the failed PreDelete hook, so a new quiescing attempt
# may already exist here. No failed attempt may leave deletion permission live.
UNINSTALL_STATE=$(kubectl -n shiftpv-system get configmap/shiftpv-uninstall-permit \
	-o jsonpath='{.data.state}' 2>/dev/null || true)
if [[ "${UNINSTALL_STATE}" == "granted" ]]; then
	echo "failed Argo CD deletion left an uninstall grant active" >&2
	exit 1
fi

CHECKSUM_AFTER_DENIAL=$(kubectl exec shiftpv-argocd-e2e -- sha256sum /data/payload | awk '{print $1}')
if [[ "${CHECKSUM_BEFORE}" != "${CHECKSUM_AFTER_DENIAL}" ]]; then
	echo "checksum mismatch after denied Argo CD Application deletion" >&2
	exit 1
fi

# Remove all blockers and the failed hook Job. Argo CD must retry, observe the
# safe state, and finish the same pending Application deletion.
kubectl delete pod shiftpv-argocd-e2e --wait=true
kubectl delete pvc shiftpv-argocd-e2e --wait=true
kubectl delete "pv/${PV_NAME}" --wait=true
kubectl delete "shiftpvvolume/${VOLUME_ID}" --ignore-not-found --wait=true
kubectl -n shiftpv-system delete job shiftpv-uninstall-guard --ignore-not-found --wait=true
kubectl -n argocd wait --for=delete application/shiftpv --timeout=5m

if kubectl get storageclass shiftpv >/dev/null 2>&1; then
	echo "Argo CD did not finish deletion after uninstall blockers were removed" >&2
	exit 1
fi
if kubectl get validatingwebhookconfiguration shiftpv-lifecycle >/dev/null 2>&1; then
	echo "Argo CD left lifecycle validation after a completed Application deletion" >&2
	exit 1
fi
test -f "${WORKER_POOL}/volumes/${VOLUME_ID}/payload"

echo "ShiftPV Argo CD uninstall guard E2E passed"
echo "ArgoCD=${ARGOCD_VERSION} PV=${PV_NAME} volume=${VOLUME_ID} checksum=${CHECKSUM_AFTER_DENIAL}"
