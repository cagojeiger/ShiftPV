#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)
# shellcheck source=test/e2e/kind/node-path.sh
source "${ROOT_DIR}/test/e2e/kind/node-path.sh"
# shellcheck source=test/e2e/kind/cleanup-journal.sh
source "${ROOT_DIR}/test/e2e/kind/cleanup-journal.sh"
CLUSTER_NAME=${CLUSTER_NAME:-shiftpv-argocd-e2e}
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0}
ARGOCD_VERSION=${ARGOCD_VERSION:-v3.5.2}
ARGOCD_MANIFEST_SHA256=${ARGOCD_MANIFEST_SHA256:-9a87f2b3e14c278f12501eb0ef5c3955b27cf05370ca425381c6a908cf85a5c5}
IMAGE_REPOSITORY=${IMAGE_REPOSITORY:-shiftpv-argocd-e2e}
IMAGE_TAG=${IMAGE_TAG:-dev}
KEEP_CLUSTER=${KEEP_CLUSTER:-0}
NODE="${CLUSTER_NAME}-worker"
POOL_PATH=/mnt/shiftpv
VOLUME_FINALIZER_POLICY=shiftpv-argocd-volume-finalizer

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

# Reinstall in Argo CD mode and create a real mounted volume. The long-running
# PreDelete hook must keep the Application and every driver component alive
# until the dependency is removed.
apply_and_wait_for_application
kubectl apply -f "${WORK_DIR}/pool.yaml"
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/argocd/workload.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-argocd-e2e --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/shiftpv-argocd-e2e --timeout=2m

PV_NAME=$(kubectl get pvc shiftpv-argocd-e2e -o jsonpath='{.spec.volumeName}')
VOLUME_ID=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
PVC_UID=$(kubectl get pvc shiftpv-argocd-e2e -o jsonpath='{.metadata.uid}')
COPY_ID=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.currentCopy.copyID}')
CHECKSUM_BEFORE=$(kubectl exec shiftpv-argocd-e2e -- sha256sum /data/payload | awk '{print $1}')
test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.spec.requestName}')" = "pvc-${PVC_UID}"
test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.spec.capacityBytes}')" = 67108864
test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.spec.initialNode}')" = "${NODE}"

# Pool deletion is fenced by a controller-owned finalizer. This Application
# test keeps the active Pool registered; directory-pool.sh proves terminating
# Pools remain until exact storage dependencies converge.
kubectl wait --for=jsonpath='{.metadata.finalizers[0]}'=shiftpv.io/pool-protection \
	shiftpvpool/worker --timeout=2m
if kubectl patch shiftpvpool/worker --type=json \
	-p='[{"op":"remove","path":"/metadata/finalizers/0"}]'; then
	echo "direct ShiftPVPool finalizer removal bypassed lifecycle admission" >&2
	exit 1
fi
if kubectl delete customresourcedefinition/shiftpvpools.shiftpv.io --wait=false; then
	echo "direct ShiftPVPool CRD deletion bypassed lifecycle admission" >&2
	exit 1
fi
if kubectl delete "shiftpvvolume/${VOLUME_ID}" --wait=false; then
	echo "direct ShiftPVVolume deletion bypassed lifecycle admission" >&2
	exit 1
fi
kubectl get shiftpvpool/worker >/dev/null
kubectl get customresourcedefinition/shiftpvpools.shiftpv.io >/dev/null
kubectl get "shiftpvvolume/${VOLUME_ID}" >/dev/null

kubectl -n argocd delete application shiftpv --wait=false
kubectl -n shiftpv-system wait --for=create job/shiftpv-uninstall-guard --timeout=2m
kubectl -n shiftpv-system wait --for=jsonpath='{.status.active}'=1 job/shiftpv-uninstall-guard --timeout=2m
test -n "$(kubectl -n argocd get application shiftpv -o jsonpath='{.metadata.deletionTimestamp}')"

kubectl -n shiftpv-system get deployment/shiftpv-controller >/dev/null
kubectl -n shiftpv-system get daemonset/shiftpv-node >/dev/null
kubectl get storageclass shiftpv >/dev/null
kubectl get validatingwebhookconfiguration shiftpv-lifecycle >/dev/null

# The guard may be between bounded attempts. It must never grant deletion while
# the mounted volume still exists.
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

# Hold the Volume finalizer for one reconciliation after its embedded cleanup
# journal settles. This makes the complete receipt and later absence proof
# observable before the Volume-owned capacity hold is released.
kubectl apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: ${VOLUME_FINALIZER_POLICY}
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [shiftpv.io]
        apiVersions: [v1alpha1]
        operations: [UPDATE]
        resources: [shiftpvvolumes]
  validations:
    - expression: "object.metadata.name != '${VOLUME_ID}' || !has(oldObject.status.cleanup) || oldObject.status.cleanup.status.phase != 'Completed' || object.metadata.finalizers.exists(finalizer, finalizer == 'shiftpv.io/volume-protection')"
      message: volume cleanup settlement observation hold
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: ${VOLUME_FINALIZER_POLICY}
spec:
  policyName: ${VOLUME_FINALIZER_POLICY}
  validationActions: [Deny]
EOF
kubectl patch "pv/${PV_NAME}" --type=merge \
	-p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl delete pod shiftpv-argocd-e2e --wait=true
kubectl delete pvc shiftpv-argocd-e2e --wait=true
wait_for_cleanup_phase "shiftpvvolume/${VOLUME_ID}" Completed 5m
assert_cleanup_journal "shiftpvvolume/${VOLUME_ID}" VolumeDelete "${VOLUME_ID}" "${COPY_ID}" ShiftPVVolume
test -n "$(kubectl -n argocd get application shiftpv -o jsonpath='{.metadata.deletionTimestamp}')"
GUARD_LOG=$(kubectl -n shiftpv-system logs job/shiftpv-uninstall-guard)
grep -Fq ShiftPVVolume <<<"${GUARD_LOG}"
grep -Fq "${VOLUME_ID}" <<<"${GUARD_LOG}"
assert_node_absent "${NODE}" "${POOL_PATH}/volumes/${VOLUME_ID}"

kubectl delete validatingadmissionpolicybinding "${VOLUME_FINALIZER_POLICY}"
kubectl delete validatingadmissionpolicy "${VOLUME_FINALIZER_POLICY}"
kubectl wait --for=delete "pv/${PV_NAME}" --timeout=5m
kubectl wait --for=delete "shiftpvvolume/${VOLUME_ID}" --timeout=2m

# The same pending Application deletion now completes without restarting its
# PreDelete hook because the Volume hold and exact copy obligation are gone.
kubectl -n argocd wait --for=delete application/shiftpv --timeout=5m

if kubectl get storageclass shiftpv >/dev/null 2>&1; then
	echo "Argo CD did not finish deletion after uninstall blockers were removed" >&2
	exit 1
fi
if kubectl get validatingwebhookconfiguration shiftpv-lifecycle >/dev/null 2>&1; then
	echo "Argo CD left lifecycle validation after a completed Application deletion" >&2
	exit 1
fi
assert_node_absent "${NODE}" "${POOL_PATH}/volumes/${VOLUME_ID}"
assert_node_absent "${NODE}" "${POOL_PATH}/.shiftpv/placements/placement-${COPY_ID}.json"
assert_node_absent "${NODE}" "${POOL_PATH}/.shiftpv/copy-${COPY_ID}.json"

echo "ShiftPV Argo CD uninstall guard E2E passed"
echo "ArgoCD=${ARGOCD_VERSION} PV=${PV_NAME} volume=${VOLUME_ID} checksum=${CHECKSUM_AFTER_DENIAL}; embedded cleanup receipt and absence proof settled before deletion"
