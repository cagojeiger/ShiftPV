#!/usr/bin/env bash
set -euo pipefail

# Run against the caller's isolated Kind cluster after mobility installation.
: "${CLUSTER_NAME:?isolated Kind cluster name required}"
: "${KUBECONFIG:?isolated kubeconfig required}"
test "$(kubectl config current-context)" = "kind-${CLUSTER_NAME}"
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)
NAMESPACE=shiftpv-completion-test
POLICY=shiftpv-completion-test
EXISTING_NAMESPACE=$(kubectl get namespace "${NAMESPACE}" --ignore-not-found -o name)
EXISTING_POLICY=$(kubectl get validatingadmissionpolicy "${POLICY}" --ignore-not-found -o name)
EXISTING_BINDING=$(kubectl get validatingadmissionpolicybinding "${POLICY}" --ignore-not-found -o name)
if [[ -n "${EXISTING_NAMESPACE}${EXISTING_POLICY}${EXISTING_BINDING}" ]]; then
	echo 'completion test resources already exist; refusing to overwrite them' >&2
	exit 1
fi

sed 's/shiftpv-mobility-test/shiftpv-completion-test/g' \
	"${ROOT_DIR}/test/e2e/kind/mobility/manifests/wffc-workload.yaml" | kubectl apply -f -
kubectl -n "${NAMESPACE}" rollout status deployment/wffc --timeout=180s
PV=$(kubectl -n "${NAMESPACE}" get pvc/wffc -o jsonpath='{.spec.volumeName}')
VOLUME=$(kubectl get "pv/${PV}" -o jsonpath='{.spec.csi.volumeHandle}')
SOURCE=$(kubectl get "shiftpvvolume/${VOLUME}" -o jsonpath='{.status.ownerNode}')
CHECKSUM=$(kubectl -n "${NAMESPACE}" exec deployment/wffc -- sha256sum /data/payload | awk '{print $1}')
kubectl patch "pv/${PV}" --type=merge -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'

kubectl apply -f - <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: ${POLICY}
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
    - apiGroups: [shiftpv.io]
      apiVersions: [v1alpha1]
      operations: [UPDATE]
      resources: [shiftpvmoves/status]
  validations:
  - expression: "object.spec.volumeID != '${VOLUME}' || !has(object.status.phase) || object.status.phase != 'Succeeded'"
    message: completion journal failure injection
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: ${POLICY}
spec:
  policyName: ${POLICY}
  validationActions: [Deny]
EOF
kubectl cordon "${SOURCE}"
deadline=$((SECONDS + 180))
MOVE=""
while ((SECONDS < deadline)); do
	MOVE=$(kubectl get "shiftpvvolume/${VOLUME}" -o jsonpath='{.status.activeMove}')
	[[ -n "${MOVE}" ]] && break
	sleep 1
done
test -n "${MOVE}"
kubectl wait "shiftpvmove/${MOVE}" --for=jsonpath='{.status.phase}'=Completing --timeout=300s
deadline=$((SECONDS + 120))
while ((SECONDS < deadline)); do
	ACTIVE=$(kubectl get "shiftpvvolume/${VOLUME}" -o jsonpath='{.status.activeMove}')
	[[ -z "${ACTIVE}" ]] && break
	sleep 1
done
test -z "${ACTIVE}"
test "$(kubectl get "shiftpvmove/${MOVE}" -o jsonpath='{.status.phase}')" = Completing
if DENIAL=$(kubectl patch "shiftpvmove/${MOVE}" --subresource=status --type=merge \
	-p '{"status":{"phase":"Succeeded"}}' --dry-run=server 2>&1); then
	echo 'completion journal fault was not enforced' >&2
	exit 1
fi
grep -Fq 'completion journal failure injection' <<<"${DENIAL}"
test "$(kubectl get "shiftpvvolume/${VOLUME}" -o jsonpath='{.status.ownerNode}')" != "${SOURCE}"
kubectl -n "${NAMESPACE}" rollout status deployment/wffc --timeout=180s
test "$(kubectl -n "${NAMESPACE}" exec deployment/wffc -- sha256sum /data/payload | awk '{print $1}')" = "${CHECKSUM}"
SOURCE_POOL=$(kubectl get shiftpvpools -o jsonpath="{.items[?(@.spec.nodeName=='${SOURCE}')].spec.mountPath}")
test -n "${SOURCE_POOL}"
docker exec "${SOURCE}" test ! -e "${SOURCE_POOL}/volumes/${VOLUME}"
CLEANUP=$(kubectl get "shiftpvmove/${MOVE}" -o jsonpath='{.status.cleanupName}')
SOURCE_COPY=$(kubectl get "shiftpvmove/${MOVE}" -o jsonpath='{.status.sourceCopy.copyID}')
test -n "${CLEANUP}"
test -n "${SOURCE_COPY}"
docker exec "${SOURCE}" test ! -e "${SOURCE_POOL}/.shiftpv/retired/${SOURCE_COPY}"
CLEANUP_JOB=$(kubectl get "shiftpvmove/${MOVE}" -o jsonpath='{.status.cleanupJobName}')
test -n "${CLEANUP_JOB}"
MOVE_UID=$(kubectl get "shiftpvmove/${MOVE}" -o jsonpath='{.metadata.uid}')
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.status.phase}')" = Completed
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.spec.approved}')" = true
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.spec.reason}')" = MoveSource
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.spec.authority.name}')" = "${MOVE}"
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.spec.authority.uid}')" = "${MOVE_UID}"
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.spec.target.copyID}')" = "${SOURCE_COPY}"
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.spec.target.volumeID}')" = "${VOLUME}"
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.status.executor.jobName}')" = "${CLEANUP_JOB}"
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.status.receipt.operationID}')" = "cleanup-${MOVE_UID}"
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.status.receipt.retired}')" = true
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.status.receipt.purged}')" = true
test -n "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.status.receipt.localReceiptDigest}')"
test -n "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.status.settledAt}')"
kubectl -n shiftpv-system delete "job/${CLEANUP_JOB}" --wait=true --timeout=120s

# Normal CSI deletion races with the still-rejected terminal journal.
kubectl delete namespace "${NAMESPACE}" --wait=true --timeout=180s
kubectl wait "pv/${PV}" --for=delete --timeout=180s
kubectl wait "shiftpvvolume/${VOLUME}" --for=delete --timeout=180s
kubectl uncordon "${SOURCE}"
kubectl -n shiftpv-system rollout restart deployment/shiftpv-controller
kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=180s
test "$(kubectl get "shiftpvmove/${MOVE}" -o jsonpath='{.status.phase}')" = Completing
kubectl delete validatingadmissionpolicybinding "${POLICY}"
kubectl delete validatingadmissionpolicy "${POLICY}"
kubectl wait "shiftpvmove/${MOVE}" --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s
JOBS=$(kubectl -n shiftpv-system get jobs -o name)
if grep -Fxq "job.batch/${CLEANUP_JOB}" <<<"${JOBS}"; then
	echo 'completion recreated source cleanup work' >&2
	exit 1
fi
test "$(kubectl get "shiftpvcleanup/${CLEANUP}" -o jsonpath='{.status.phase}')" = Completed
echo "completion journal recovery E2E passed: move=${MOVE} volume=${VOLUME} checksum=${CHECKSUM}; CSI deletion, Job removal and controller restart"
