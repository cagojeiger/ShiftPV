#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${WORK_DIR:?WORK_DIR is required}"

SOURCE_NODE="${CLUSTER_NAME}-worker"
DESTINATION_NODE="${CLUSTER_NAME}-worker2"
BASELINE_CHART_VERSION=0.1.3

helm repo add shiftpv-upgrade-source https://cagojeiger.github.io/ShiftPV --force-update
helm repo update shiftpv-upgrade-source
helm upgrade --install shiftpv shiftpv-upgrade-source/shiftpv \
	--version "${BASELINE_CHART_VERSION}" \
	--namespace shiftpv-system \
	--create-namespace \
	--set-string 'node.nodeSelector.shiftpv\.io/storage-node=true' \
	--set storageClass.defaultClass=true \
	--wait \
	--timeout 8m

if [[ -n "$(kubectl get crd/shiftpvpools.shiftpv.io -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.capacity}')" ]]; then
	echo "baseline chart unexpectedly contains the target Pool capacity schema" >&2
	exit 1
fi

kubectl apply -f - <<EOF
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: worker-a
spec:
  nodeName: ${SOURCE_NODE}
  mountPath: /mnt/shiftpv
---
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: worker-b
spec:
  nodeName: ${DESTINATION_NODE}
  mountPath: /srv/shiftpv-b
EOF

kubectl apply -f "${ROOT_DIR}/test/e2e/kind/pvc.yaml"
kubectl apply -f "${ROOT_DIR}/test/e2e/kind/pod.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-e2e --timeout=5m
BASELINE_PV=$(kubectl get pvc/shiftpv-e2e -o jsonpath='{.spec.volumeName}')
BASELINE_VOLUME=$(kubectl get "pv/${BASELINE_PV}" -o jsonpath='{.spec.csi.volumeHandle}')
BASELINE_CHECKSUM=$(kubectl exec shiftpv-e2e -- sha256sum /data/payload | awk '{print $1}')

# Helm deliberately does not upgrade files under charts/*/crds. Operators must
# apply the new schemas before starting the new controller.
kubectl apply --server-side --field-manager=shiftpv-crd-upgrade \
	--force-conflicts \
	-f "${ROOT_DIR}/charts/shiftpv/crds"
kubectl wait --for=condition=Established crd/shiftpvpools.shiftpv.io crd/shiftpvmoves.shiftpv.io --timeout=2m

for pool in worker-a worker-b; do
	kubectl patch "shiftpvpool/${pool}" --type=merge -p '{"spec":{"capacity":{"limit":"10Gi"}}}'
done

helm upgrade shiftpv "${ROOT_DIR}/charts/shiftpv" \
	--namespace shiftpv-system \
	--values "${ROOT_DIR}/test/e2e/kind/values.yaml" \
	--wait \
	--timeout 8m
kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=5m
kubectl -n shiftpv-system rollout status daemonset/shiftpv-node --timeout=5m

test "$(kubectl get shiftpvpool/worker-a -o jsonpath='{.spec.capacity.limit}')" = 10Gi
test "$(kubectl get crd/shiftpvmoves.shiftpv.io -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.status.properties.capacityApproved.type}')" = boolean
test "$(kubectl get storageclass/shiftpv -o jsonpath='{.parameters.shiftpv\.io/capacity-enforcement}')" = none
test "$(kubectl -n shiftpv-system get deployment/shiftpv-controller -o jsonpath='{.spec.template.spec.containers[?(@.name=="shiftpv-controller")].image}')" = shiftpv:dev
test "$(kubectl -n shiftpv-system get daemonset/shiftpv-node -o jsonpath='{.spec.template.spec.containers[?(@.name=="shiftpv-node")].image}')" = shiftpv:dev
test "$(kubectl get "shiftpvvolume/${BASELINE_VOLUME}" -o jsonpath='{.status.phase}')" = Ready
test "$(kubectl exec shiftpv-e2e -- sha256sum /data/payload | awk '{print $1}')" = "${BASELINE_CHECKSUM}"

sed 's/shiftpv-e2e/shiftpv-upgrade-new/g' "${ROOT_DIR}/test/e2e/kind/pvc.yaml" >"${WORK_DIR}/upgrade-pvc.yaml"
sed 's/shiftpv-e2e/shiftpv-upgrade-new/g' "${ROOT_DIR}/test/e2e/kind/pod.yaml" >"${WORK_DIR}/upgrade-pod.yaml"
kubectl apply -f "${WORK_DIR}/upgrade-pvc.yaml"
kubectl apply -f "${WORK_DIR}/upgrade-pod.yaml"
kubectl wait --for=condition=Ready pod/shiftpv-upgrade-new --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/shiftpv-upgrade-new --timeout=2m

echo "ShiftPV upgrade passed: chart=${BASELINE_CHART_VERSION} volume=${BASELINE_VOLUME} checksum=${BASELINE_CHECKSUM}"
