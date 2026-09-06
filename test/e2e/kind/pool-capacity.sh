#!/usr/bin/env bash
set -euo pipefail

: "${CLUSTER_NAME:?CLUSTER_NAME is required}"

CAPACITY_NODE="${CLUSTER_NAME}-worker"
CAPACITY_POOL=worker-a
CAPACITY_PATH=/mnt/shiftpv
PHYSICAL_NAME=shiftpv-capacity-physical
LOGICAL_NAME=shiftpv-capacity-logical
PHYSICAL_PV=
LOGICAL_PV=

reservation_count() {
	kubectl -n shiftpv-system get configmap \
		-l app.kubernetes.io/name=shiftpv,app.kubernetes.io/component=volume-reservation \
		-o name | wc -l | tr -d ' '
}

wait_for_reservation_count() {
	local expected=$1
	local attempt
	for ((attempt = 0; attempt < 120; attempt++)); do
		if [[ "$(reservation_count)" == "${expected}" ]]; then
			return
		fi
		sleep 1
	done
	echo "reservation count did not become ${expected}" >&2
	return 1
}

wait_for_resource_exhausted() {
	local name=$1
	local attempt messages
	for ((attempt = 0; attempt < 120; attempt++)); do
		messages=$(kubectl get events \
			--field-selector "involvedObject.kind=PersistentVolumeClaim,involvedObject.name=${name}" \
			-o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null || true)
		if grep -Fq 'code = ResourceExhausted' <<<"${messages}"; then
			return
		fi
		sleep 1
	done
	echo "PersistentVolumeClaim/${name} did not report ResourceExhausted" >&2
	return 1
}

create_workload() {
	local name=$1
	local size=$2
	kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${name}
spec:
  storageClassName: shiftpv-capacity-test
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  resources:
    requests:
      storage: ${size}
---
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
spec:
  nodeSelector:
    kubernetes.io/hostname: ${CAPACITY_NODE}
  containers:
    - name: workload
      image: busybox:1.37
      command: ["sh", "-c", "sleep 3600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${name}
EOF
}

cleanup_capacity_test() {
	kubectl delete pod "${PHYSICAL_NAME}" "${LOGICAL_NAME}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kubectl delete pvc "${PHYSICAL_NAME}" "${LOGICAL_NAME}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	if [[ -n "${PHYSICAL_PV}" ]]; then
		kubectl wait --for=delete "pv/${PHYSICAL_PV}" --timeout=2m >/dev/null 2>&1 || true
	fi
	if [[ -n "${LOGICAL_PV}" ]]; then
		kubectl wait --for=delete "pv/${LOGICAL_PV}" --timeout=2m >/dev/null 2>&1 || true
	fi
	kubectl delete storageclass shiftpv-capacity-test --ignore-not-found --wait=true >/dev/null 2>&1 || true
	docker exec "${CAPACITY_NODE}" sh -c "rm -f '${CAPACITY_PATH}/external-fill'; mountpoint -q '${CAPACITY_PATH}' && umount '${CAPACITY_PATH}' || true" >/dev/null 2>&1 || true
	kubectl patch shiftpvpool "${CAPACITY_POOL}" --type=merge -p '{"spec":{"capacity":{"limit":"10Gi"}}}' >/dev/null 2>&1 || true
}
trap cleanup_capacity_test EXIT

kubectl patch shiftpvpool "${CAPACITY_POOL}" --type=merge -p '{"spec":{"capacity":{"limit":"128Mi"}}}' >/dev/null
docker exec "${CAPACITY_NODE}" mount -t tmpfs -o size=128m shiftpv-capacity "${CAPACITY_PATH}"
docker exec "${CAPACITY_NODE}" sh -c "dd if=/dev/zero of='${CAPACITY_PATH}/external-fill' bs=1M count=80 >/dev/null 2>&1"

kubectl apply -f - <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: shiftpv-capacity-test
provisioner: csi.shiftpv.io
reclaimPolicy: Delete
allowVolumeExpansion: false
volumeBindingMode: WaitForFirstConsumer
EOF

# statfs must see bytes consumed outside ShiftPV and reject before reserving.
create_workload "${PHYSICAL_NAME}" 64Mi
wait_for_resource_exhausted "${PHYSICAL_NAME}"
wait_for_reservation_count 0

# Removing the external file makes the same pending claim converge.
docker exec "${CAPACITY_NODE}" rm -f "${CAPACITY_PATH}/external-fill"
kubectl wait --for=condition=Ready "pod/${PHYSICAL_NAME}" --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound "pvc/${PHYSICAL_NAME}" --timeout=2m
PHYSICAL_PV=$(kubectl get pvc "${PHYSICAL_NAME}" -o jsonpath='{.spec.volumeName}')
wait_for_reservation_count 1

# An empty 64Mi PVC uses almost no bytes, but its reservation must still leave
# only 64Mi of the Pool limit and reject a new 80Mi request.
create_workload "${LOGICAL_NAME}" 80Mi
wait_for_resource_exhausted "${LOGICAL_NAME}"
wait_for_reservation_count 1

# Releasing the first reservation allows the pending second claim to converge.
kubectl delete pod "${PHYSICAL_NAME}" --wait=true
kubectl delete pvc "${PHYSICAL_NAME}" --wait=true
kubectl wait --for=delete "pv/${PHYSICAL_PV}" --timeout=2m
PHYSICAL_PV=
wait_for_reservation_count 0
kubectl wait --for=condition=Ready "pod/${LOGICAL_NAME}" --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound "pvc/${LOGICAL_NAME}" --timeout=2m
LOGICAL_PV=$(kubectl get pvc "${LOGICAL_NAME}" -o jsonpath='{.spec.volumeName}')
wait_for_reservation_count 1

kubectl delete pod "${LOGICAL_NAME}" --wait=true
kubectl delete pvc "${LOGICAL_NAME}" --wait=true
kubectl wait --for=delete "pv/${LOGICAL_PV}" --timeout=2m
LOGICAL_PV=
wait_for_reservation_count 0

trap - EXIT
cleanup_capacity_test
echo "ShiftPV Pool filesystem capacity admission passed"
