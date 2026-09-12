#!/usr/bin/env bash
set -euo pipefail

: "${ROOT_DIR:=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)}"
# shellcheck source=test/e2e/kind/node-path.sh
source "${ROOT_DIR}/test/e2e/kind/node-path.sh"

: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${WORKER_A_POOL:?WORKER_A_POOL is required}"

NAMESPACE=shiftpv-orphan-e2e
NODE="${CLUSTER_NAME}-worker"
POOL=worker-a
POOL_PATH=/mnt/shiftpv
MOUNT_TARGET=/var/lib/kubelet/pods/shiftpv-orphan-probe/volumes/kubernetes.io~csi/shiftpv/mount
PV_NAME=
VOLUME_ID=
CLEANUP_NAME=
MOUNTED=0
OLD_POOL_UID=

cleanup() {
	if [[ "${MOUNTED}" == "1" ]]; then
		docker exec "${NODE}" umount "${MOUNT_TARGET}" >/dev/null 2>&1 || true
	fi
	kubectl delete namespace "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
}
trap cleanup EXIT

cleanup_for_volume() {
	kubectl get shiftpvcleanups -o json | jq -r --arg volume "${VOLUME_ID}" \
		'[.items[] | select(.spec.target.volumeID == $volume) | .metadata.name] | first // ""'
}

wait_for_inventory_publication() {
	local expected=$1 deadline=$((SECONDS + 120))
	while ((SECONDS < deadline)); do
		if kubectl get "shiftpvpool/${POOL}" -o json | jq -e --arg volume "${VOLUME_ID}" --argjson expected "${expected}" \
			'.status.inventory.valid == true and any(.status.inventory.copies[]?; .identity.volumeID == $volume and (.published // false) == $expected and .present == true and (.problem // "") == "")' >/dev/null; then
			return
		fi
		sleep 1
	done
	echo "Pool inventory did not observe published=${expected} for ${VOLUME_ID}" >&2
	kubectl get "shiftpvpool/${POOL}" -o yaml >&2 || true
	return 1
}

kubectl create namespace "${NAMESPACE}"
kubectl -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  storageClassName: shiftpv
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  resources:
    requests:
      storage: 8Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: writer
spec:
  nodeSelector:
    kubernetes.io/hostname: ${NODE}
  restartPolicy: Never
  containers:
    - name: writer
      image: busybox:1.37
      command: [sh, -ec]
      args:
        - printf 'ShiftPV exact orphan cleanup\n' > /data/payload; sleep 3600
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data
EOF
kubectl -n "${NAMESPACE}" wait --for=condition=Ready pod/writer --timeout=5m
PV_NAME=$(kubectl -n "${NAMESPACE}" get pvc/data -o jsonpath='{.spec.volumeName}')
VOLUME_ID=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
VOLUME_UID=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.metadata.uid}')
COPY_ID=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.currentCopy.copyID}')
RESERVATION_UID=$(kubectl -n shiftpv-system get "configmap/${VOLUME_ID}" -o jsonpath='{.metadata.uid}')
CONTROLLER_SERVICE_ACCOUNT=$(kubectl -n shiftpv-system get deployment/shiftpv-controller -o jsonpath='{.spec.template.spec.serviceAccountName}')
CHECKSUM=$(kubectl -n "${NAMESPACE}" exec writer -- sha256sum /data/payload | awk '{print $1}')
test "${CHECKSUM}" = "$(docker exec "${NODE}" sha256sum "${POOL_PATH}/volumes/${VOLUME_ID}/payload" | awk '{print $1}')"

kubectl -n "${NAMESPACE}" delete pod/writer --wait=true
kubectl -n "${NAMESPACE}" delete pvc/data --wait=true
kubectl wait --for=jsonpath='{.status.phase}'=Released "pv/${PV_NAME}" --timeout=2m

for _ in {1..30}; do
	if docker exec "${NODE}" sh -ec 'mkdir -p "$2" && mount --bind "$1" "$2"' sh \
		"${POOL_PATH}/volumes/${VOLUME_ID}" "${MOUNT_TARGET}" >/dev/null 2>&1; then
		MOUNTED=1
		break
	fi
	sleep 1
done
if [[ "${MOUNTED}" != "1" ]]; then
	echo "could not establish the synthetic kubelet publication mount" >&2
	exit 1
fi
wait_for_inventory_publication true

kubectl --as="system:serviceaccount:shiftpv-system:${CONTROLLER_SERVICE_ACCOUNT}" \
	delete "shiftpvvolume/${VOLUME_ID}" --wait=true
sleep 35
if [[ -n "$(cleanup_for_volume)" ]]; then
	echo "orphan cleanup was discovered while Retain PersistentVolume ${PV_NAME} still existed" >&2
	exit 1
fi
assert_node_file "${NODE}" "${POOL_PATH}/volumes/${VOLUME_ID}/payload"

kubectl delete "pv/${PV_NAME}" --wait=true
deadline=$((SECONDS + 120))
while ((SECONDS < deadline)); do
	CLEANUP_NAME=$(cleanup_for_volume)
	[[ -n "${CLEANUP_NAME}" ]] && break
	sleep 1
done
if [[ -z "${CLEANUP_NAME}" ]]; then
	echo "exact orphan cleanup contract was not discovered for ${VOLUME_ID}" >&2
	exit 1
fi
test "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.spec.approved}')" = false
test "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.spec.target.volumeUID}')" = "${VOLUME_UID}"
test "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.spec.target.copyID}')" = "${COPY_ID}"
test "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.spec.reservationUID}')" = "${RESERVATION_UID}"
kubectl wait --for=jsonpath='{.status.reason}'=CopyMounted "shiftpvcleanup/${CLEANUP_NAME}" --timeout=2m

kubectl patch "shiftpvcleanup/${CLEANUP_NAME}" --type merge -p '{"spec":{"approved":true}}'
sleep 35
test "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.status.phase}')" = NeedsReview
test "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.status.reason}')" = CopyMounted
if kubectl -n shiftpv-system get "job/${CLEANUP_NAME}-effect" >/dev/null 2>&1; then
	echo "cleanup Job started while the exact orphan copy was mounted" >&2
	exit 1
fi
assert_node_file "${NODE}" "${POOL_PATH}/volumes/${VOLUME_ID}/payload"
kubectl -n shiftpv-system get "configmap/${VOLUME_ID}" >/dev/null

# Keep the Pool terminating while the approved orphan cleanup runs. New
# placement is closed, but the exact cleanup must remain executable so the
# physical copy blocker and Pool finalizer can converge.
OLD_POOL_UID=$(kubectl get "shiftpvpool/${POOL}" -o jsonpath='{.metadata.uid}')
kubectl delete "shiftpvpool/${POOL}" --wait=false
kubectl wait --for=condition=Ready=false "shiftpvpool/${POOL}" --timeout=2m
test -n "$(kubectl get "shiftpvpool/${POOL}" -o jsonpath='{.metadata.deletionTimestamp}')"
test "$(kubectl get "shiftpvpool/${POOL}" -o jsonpath='{.metadata.finalizers[0]}')" = shiftpv.io/pool-protection
test "$(kubectl get "shiftpvpool/${POOL}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')" = PoolDeregistering

docker exec "${NODE}" umount "${MOUNT_TARGET}"
docker exec "${NODE}" rmdir "${MOUNT_TARGET}"
MOUNTED=0
wait_for_inventory_publication false
kubectl wait --for=jsonpath='{.status.phase}'=Completed "shiftpvcleanup/${CLEANUP_NAME}" --timeout=4m
test "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.status.receipt.operationID}')" = "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.spec.operationID}')"
test "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.status.receipt.purged}')" = true
test -n "$(kubectl get "shiftpvcleanup/${CLEANUP_NAME}" -o jsonpath='{.status.settledAt}')"
kubectl -n shiftpv-system wait --for=condition=complete "job/${CLEANUP_NAME}-effect" --timeout=2m
assert_node_absent "${NODE}" "${POOL_PATH}/volumes/${VOLUME_ID}"
assert_node_absent "${NODE}" "${POOL_PATH}/.shiftpv/placements/placement-${COPY_ID}.json"
assert_node_absent "${NODE}" "${POOL_PATH}/.shiftpv/copy-${COPY_ID}.json"
if kubectl -n shiftpv-system get "configmap/${VOLUME_ID}" >/dev/null 2>&1; then
	echo "exact orphan reservation remains after settled cleanup" >&2
	exit 1
fi
kubectl wait --for=delete "shiftpvpool/${POOL}" --timeout=2m

kubectl apply -f - <<EOF
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: ${POOL}
spec:
  nodeName: ${NODE}
  mountPath: ${POOL_PATH}
  capacity:
    limit: 10Gi
EOF
kubectl wait --for=condition=Ready "shiftpvpool/${POOL}" --timeout=2m
kubectl wait --for=jsonpath='{.status.inventory.valid}'=true "shiftpvpool/${POOL}" --timeout=2m
kubectl wait --for=jsonpath='{.metadata.finalizers[0]}'=shiftpv.io/pool-protection "shiftpvpool/${POOL}" --timeout=2m
NEW_POOL_UID=$(kubectl get "shiftpvpool/${POOL}" -o jsonpath='{.metadata.uid}')
if [[ "${NEW_POOL_UID}" == "${OLD_POOL_UID}" ]]; then
	echo "re-registered Pool kept the deleted identity: ${NEW_POOL_UID}" >&2
	exit 1
fi

echo "ShiftPV exact orphan cleanup E2E passed during Pool deregistration: volume=${VOLUME_ID} cleanup=${CLEANUP_NAME} checksum=${CHECKSUM}"
