#!/usr/bin/env bash
set -euo pipefail

: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${WORK_DIR:?WORK_DIR is required}"

POOL_A_NAME=ordinary-directory-a
POOL_B_NAME=ordinary-directory-b
POOL_A_NODE="${CLUSTER_NAME}-worker"
POOL_B_NODE="${CLUSTER_NAME}-worker2"
POOL_A_PATH=/var/lib/shiftpv-directory-pool-a
POOL_B_PATH=/var/lib/shiftpv-directory-pool-b
STORAGE_CLASS=shiftpv-directory-test
WORKLOAD=shiftpv-directory-test
MOBILITY_NAMESPACE=shiftpv-directory-mobility
PV_NAME=
VOLUME_ID=

restore_default_pool() {
	kubectl delete pod "${WORKLOAD}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kubectl delete pvc "${WORKLOAD}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kubectl delete namespace "${MOBILITY_NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	if [[ -n "${PV_NAME}" ]]; then
		kubectl wait --for=delete "pv/${PV_NAME}" --timeout=2m >/dev/null 2>&1 || true
	fi
	kubectl delete storageclass "${STORAGE_CLASS}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kubectl delete shiftpvpool "${POOL_A_NAME}" "${POOL_B_NAME}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kubectl uncordon "${POOL_A_NODE}" >/dev/null 2>&1 || true
	kubectl uncordon "${POOL_B_NODE}" >/dev/null 2>&1 || true
	kubectl apply -f "${WORK_DIR}/pools.yaml" >/dev/null 2>&1 || true
}
trap restore_default_pool EXIT

# Register one ordinary root-filesystem directory Pool per Kind node.
kubectl delete shiftpvpool worker-a worker-b --wait=true
docker exec "${POOL_A_NODE}" test ! -e "${POOL_A_PATH}"
docker exec "${POOL_B_NODE}" test ! -e "${POOL_B_PATH}"

kubectl apply -f - <<EOF
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: ${POOL_A_NAME}
spec:
  nodeName: ${POOL_A_NODE}
  mountPath: ${POOL_A_PATH}
  capacity:
    limit: 512Mi
---
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVPool
metadata:
  name: ${POOL_B_NAME}
spec:
  nodeName: ${POOL_B_NODE}
  mountPath: ${POOL_B_PATH}
  capacity:
    limit: 512Mi
EOF
kubectl wait --for=condition=Ready=false "shiftpvpool/${POOL_A_NAME}" --timeout=2m
MISSING_REASON=$(kubectl get "shiftpvpool/${POOL_A_NAME}" \
	-o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')
if [[ "${MISSING_REASON}" != "PathMissing" ]]; then
	echo "missing ordinary directory reported unexpected reason: ${MISSING_REASON}" >&2
	exit 1
fi
docker exec "${POOL_A_NODE}" test ! -e "${POOL_A_PATH}"

for node_path in "${POOL_A_NODE}:${POOL_A_PATH}" "${POOL_B_NODE}:${POOL_B_PATH}"; do
	node=${node_path%%:*}
	path=${node_path#*:}
	docker exec "${node}" mkdir -p "${path}"
	if docker exec "${node}" mountpoint -q "${path}"; then
		echo "ordinary-directory test path unexpectedly is a mount point: ${node}:${path}" >&2
		exit 1
	fi
done
kubectl wait --for=condition=Ready "shiftpvpool/${POOL_A_NAME}" --timeout=2m
kubectl wait --for=condition=Ready "shiftpvpool/${POOL_B_NAME}" --timeout=2m

ACCESSIBLE_STATUS=$(kubectl get "shiftpvpool/${POOL_A_NAME}" \
	-o jsonpath='{.status.conditions[?(@.type=="Accessible")].status}')
ACCESSIBLE_REASON=$(kubectl get "shiftpvpool/${POOL_A_NAME}" \
	-o jsonpath='{.status.conditions[?(@.type=="Accessible")].reason}')
MOUNTED_STATUS=$(kubectl get "shiftpvpool/${POOL_A_NAME}" \
	-o jsonpath='{.status.conditions[?(@.type=="Mounted")].status}')
if [[ "${ACCESSIBLE_STATUS}" != "True" || "${ACCESSIBLE_REASON}" != "DirectoryAccessible" ]]; then
	echo "ordinary directory was not reported accessible: status=${ACCESSIBLE_STATUS} reason=${ACCESSIBLE_REASON}" >&2
	exit 1
fi
if [[ -n "${MOUNTED_STATUS}" ]]; then
	echo "obsolete Mounted condition remains on the Pool: ${MOUNTED_STATUS}" >&2
	exit 1
fi

kubectl apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ${STORAGE_CLASS}
provisioner: csi.shiftpv.io
reclaimPolicy: Delete
allowVolumeExpansion: false
volumeBindingMode: WaitForFirstConsumer
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${WORKLOAD}
spec:
  storageClassName: ${STORAGE_CLASS}
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  resources:
    requests:
      storage: 8Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: ${WORKLOAD}
spec:
  nodeSelector:
    kubernetes.io/hostname: ${POOL_A_NODE}
  restartPolicy: Never
  containers:
    - name: writer
      image: busybox:1.37
      command:
        - sh
        - -ec
        - |
          printf 'ShiftPV ordinary directory Pool\n' > /data/payload
          sleep 3600
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${WORKLOAD}
EOF

kubectl wait --for=condition=Ready "pod/${WORKLOAD}" --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound "pvc/${WORKLOAD}" --timeout=2m
PV_NAME=$(kubectl get "pvc/${WORKLOAD}" -o jsonpath='{.spec.volumeName}')
VOLUME_ID=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
PV_DRIVER=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.driver}')
if [[ "${PV_DRIVER}" != "csi.shiftpv.io" ]]; then
	echo "ordinary-directory PVC used unexpected driver: ${PV_DRIVER}" >&2
	exit 1
fi
kubectl exec "${WORKLOAD}" -- grep -Fx 'ShiftPV ordinary directory Pool' /data/payload
docker exec "${POOL_A_NODE}" grep -Fx \
	'ShiftPV ordinary directory Pool' "${POOL_A_PATH}/volumes/${VOLUME_ID}/payload"

kubectl patch pv "${PV_NAME}" --type=merge \
	-p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
kubectl delete pod "${WORKLOAD}" --wait=true
for _ in {1..60}; do
	PUBLISHED_NODES=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.publishedNodes[*]}')
	[[ -z "${PUBLISHED_NODES}" ]] && break
	sleep 1
done
if [[ -n "${PUBLISHED_NODES}" ]]; then
	echo "retirement requires an unpublished volume: ${PUBLISHED_NODES}" >&2
	exit 1
fi
kubectl wait --for=jsonpath='{.status.phase}'=Ready "shiftpvvolume/${VOLUME_ID}" --timeout=2m
test -z "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')"
kubectl delete pvc "${WORKLOAD}" --wait=true
kubectl wait --for=jsonpath='{.status.phase}'=Released "pv/${PV_NAME}" --timeout=2m
kubectl -n shiftpv-system get "configmap/${VOLUME_ID}" >/dev/null
kubectl get "shiftpvvolume/${VOLUME_ID}" >/dev/null
docker exec "${POOL_A_NODE}" grep -Fx \
	'ShiftPV ordinary directory Pool' "${POOL_A_PATH}/volumes/${VOLUME_ID}/payload"

kubectl patch pv "${PV_NAME}" --type=merge \
	-p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
kubectl wait --for=delete "pv/${PV_NAME}" --timeout=2m
kubectl -n shiftpv-system wait --for=delete "configmap/${VOLUME_ID}" --timeout=2m
kubectl wait --for=delete "shiftpvvolume/${VOLUME_ID}" --timeout=2m
docker exec "${POOL_A_NODE}" test ! -e "${POOL_A_PATH}/volumes/${VOLUME_ID}"
PV_NAME=

# Prove that mobility composes with the same ordinary-directory Pool contract.
# Cordon worker B during initial placement so worker A is the deterministic
# source, then cordon A and wait for the controller's cold move to B.
kubectl cordon "${POOL_B_NODE}"
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${MOBILITY_NAMESPACE}
  labels:
    shiftpv.io/admission: enabled
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: ${MOBILITY_NAMESPACE}
spec:
  storageClassName: ${STORAGE_CLASS}
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  resources:
    requests:
      storage: 8Mi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: writer
  namespace: ${MOBILITY_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: shiftpv-directory-mobility
  template:
    metadata:
      labels:
        app: shiftpv-directory-mobility
    spec:
      containers:
        - name: writer
          image: busybox:1.37
          command:
            - sh
            - -ec
            - |
              if [ ! -f /data/payload ]; then
                printf 'ShiftPV ordinary directory mobility\n' > /data/payload
              fi
              sleep 3600
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: data
EOF
kubectl -n "${MOBILITY_NAMESPACE}" rollout status deployment/writer --timeout=5m
MOBILITY_POD=$(kubectl -n "${MOBILITY_NAMESPACE}" get pod \
	-l app=shiftpv-directory-mobility -o jsonpath='{.items[0].metadata.name}')
SOURCE_NODE=$(kubectl -n "${MOBILITY_NAMESPACE}" get "pod/${MOBILITY_POD}" -o jsonpath='{.spec.nodeName}')
if [[ "${SOURCE_NODE}" != "${POOL_A_NODE}" ]]; then
	echo "ordinary-directory mobility used unexpected source: ${SOURCE_NODE}" >&2
	exit 1
fi
MOBILITY_PV=$(kubectl -n "${MOBILITY_NAMESPACE}" get pvc/data -o jsonpath='{.spec.volumeName}')
MOBILITY_VOLUME=$(kubectl get "pv/${MOBILITY_PV}" -o jsonpath='{.spec.csi.volumeHandle}')
CHECKSUM_BEFORE=$(kubectl -n "${MOBILITY_NAMESPACE}" exec "${MOBILITY_POD}" -- \
	sha256sum /data/payload | awk '{print $1}')

kubectl uncordon "${POOL_B_NODE}"
kubectl cordon "${POOL_A_NODE}"
MOVE_NAME=
MOVE_PHASE=
for _ in {1..600}; do
	MOVE_NAME=$(kubectl get shiftpvmoves \
		-o jsonpath="{.items[?(@.spec.volumeID=='${MOBILITY_VOLUME}')].metadata.name}" 2>/dev/null || true)
	if [[ -n "${MOVE_NAME}" ]]; then
		MOVE_PHASE=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		[[ "${MOVE_PHASE}" == Succeeded || "${MOVE_PHASE}" == Blocked ]] && break
	fi
	sleep 1
done
if [[ "${MOVE_PHASE}" != Succeeded ]]; then
	echo "ordinary-directory mobility did not succeed: move=${MOVE_NAME} phase=${MOVE_PHASE}" >&2
	[[ -n "${MOVE_NAME}" ]] && kubectl get "shiftpvmove/${MOVE_NAME}" -o yaml >&2
	exit 1
fi
kubectl -n "${MOBILITY_NAMESPACE}" rollout status deployment/writer --timeout=5m
MOBILITY_POD=$(kubectl -n "${MOBILITY_NAMESPACE}" get pod \
	-l app=shiftpv-directory-mobility -o jsonpath='{.items[0].metadata.name}')
DESTINATION_NODE=$(kubectl -n "${MOBILITY_NAMESPACE}" get "pod/${MOBILITY_POD}" -o jsonpath='{.spec.nodeName}')
CHECKSUM_AFTER=$(kubectl -n "${MOBILITY_NAMESPACE}" exec "${MOBILITY_POD}" -- \
	sha256sum /data/payload | awk '{print $1}')
if [[ "${DESTINATION_NODE}" != "${POOL_B_NODE}" || "${CHECKSUM_AFTER}" != "${CHECKSUM_BEFORE}" ]]; then
	echo "ordinary-directory mobility validation failed: destination=${DESTINATION_NODE}" >&2
	exit 1
fi
if [[ "$(kubectl get "shiftpvvolume/${MOBILITY_VOLUME}" -o jsonpath='{.status.ownerNode}')" != "${POOL_B_NODE}" ]]; then
	echo "ordinary-directory mobility owner was not committed to destination" >&2
	exit 1
fi
docker exec "${POOL_B_NODE}" grep -Fx \
	'ShiftPV ordinary directory mobility' "${POOL_B_PATH}/volumes/${MOBILITY_VOLUME}/payload"
docker exec "${POOL_A_NODE}" test ! -e "${POOL_A_PATH}/volumes/${MOBILITY_VOLUME}"
docker exec "${POOL_A_NODE}" test ! -e "${POOL_A_PATH}/.shiftpv/retired/${MOVE_NAME}"

kubectl uncordon "${POOL_A_NODE}"
kubectl -n "${MOBILITY_NAMESPACE}" delete deployment/writer --wait=true
kubectl -n "${MOBILITY_NAMESPACE}" delete pvc/data --wait=true
kubectl wait --for=delete "pv/${MOBILITY_PV}" --timeout=2m
kubectl -n shiftpv-system wait --for=delete "configmap/${MOBILITY_VOLUME}" --timeout=2m
kubectl delete namespace "${MOBILITY_NAMESPACE}" --wait=true

trap - EXIT
restore_default_pool
kubectl wait --for=condition=Ready shiftpvpool --all --timeout=2m
echo "ShiftPV ordinary directory Pool and mobility passed: source=${POOL_A_NODE} destination=${POOL_B_NODE} volume=${MOBILITY_VOLUME} move=${MOVE_NAME}"
