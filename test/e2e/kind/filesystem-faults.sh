#!/usr/bin/env bash
set -euo pipefail

TEST_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
: "${CLUSTER_NAME:?CLUSTER_NAME is required}"
: "${WORKER_B_POOL:?WORKER_B_POOL is required}"

FAULT_NODE="${CLUSTER_NAME}-worker2"
FAULT_POOL_PATH=/srv/shiftpv-b
MOUNT_STATE=normal

restore_pool_mount() {
  case "${MOUNT_STATE}" in
    enospc | tmpfs_rw)
      docker exec "${FAULT_NODE}" umount "${FAULT_POOL_PATH}" >/dev/null 2>&1 || true
      ;;
    tmpfs_readonly)
      docker exec "${FAULT_NODE}" mount -o remount,rw "${FAULT_POOL_PATH}" >/dev/null 2>&1 || true
      docker exec "${FAULT_NODE}" umount "${FAULT_POOL_PATH}" >/dev/null 2>&1 || true
      ;;
  esac
  MOUNT_STATE=normal
}
trap restore_pool_mount EXIT

wait_for_reservation() {
  local request_name=$1
  local attempt
  RESERVATION_NAME=""
  for ((attempt = 0; attempt < 120; attempt++)); do
    RESERVATION_NAME=$(kubectl -n shiftpv-system get configmap \
      -l app.kubernetes.io/component=volume-reservation \
      -o custom-columns=NAME:.metadata.name,REQUEST:.data.requestName \
      --no-headers 2>/dev/null | awk -v request="${request_name}" '$2 == request { print $1 }')
    if [[ -n "${RESERVATION_NAME}" ]]; then
      return
    fi
    sleep 1
  done
  echo "reservation for ${request_name} was not created" >&2
  exit 1
}

wait_for_unavailable_event() {
  local kind=$1
  local name=$2
  local attempt messages
  for ((attempt = 0; attempt < 120; attempt++)); do
    messages=$(kubectl get events \
      --field-selector "involvedObject.kind=${kind},involvedObject.name=${name}" \
      -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null || true)
    if grep -Fq 'code = Unavailable' <<<"${messages}"; then
      return
    fi
    sleep 1
  done
  echo "${kind}/${name} did not report a retryable Unavailable error" >&2
  kubectl get events \
    --field-selector "involvedObject.kind=${kind},involvedObject.name=${name}" >&2 || true
  exit 1
}

# Overlay the fault worker pool with a tiny tmpfs and exhaust its inodes. The
# helper Pod can still mount the pool, but mkdir for the volume must hit ENOSPC.
docker exec "${FAULT_NODE}" mount \
  -t tmpfs -o size=1m,nr_inodes=8 shiftpv-enospc "${FAULT_POOL_PATH}"
MOUNT_STATE=enospc
docker exec "${FAULT_NODE}" sh -ec '
  mkdir -p /srv/shiftpv-b/volumes
  index=0
  while [ "${index}" -lt 1000 ] && touch "/srv/shiftpv-b/fill-${index}" 2>/dev/null; do
    index=$((index + 1))
  done
  if [ "${index}" -eq 1000 ]; then
    echo "tmpfs inode limit was not enforced" >&2
    exit 1
  fi
  if mkdir /srv/shiftpv-b/volumes/probe 2>/dev/null; then
    echo "failed to exhaust tmpfs inodes" >&2
    exit 1
  fi
'

kubectl apply -f "${TEST_DIR}/filesystem-fault-storageclass.yaml"
kubectl apply -f "${TEST_DIR}/filesystem-fault-pvc.yaml"
PVC_UID=$(kubectl get pvc shiftpv-filesystem-fault -o jsonpath='{.metadata.uid}')
kubectl apply -f "${TEST_DIR}/filesystem-fault-pod.yaml"
wait_for_reservation "pvc-${PVC_UID}"
wait_for_unavailable_event PersistentVolumeClaim shiftpv-filesystem-fault

PVC_PHASE=$(kubectl get pvc shiftpv-filesystem-fault -o jsonpath='{.status.phase}')
if [[ "${PVC_PHASE}" != "Pending" ]]; then
  echo "ENOSPC PVC unexpectedly left Pending: ${PVC_PHASE}" >&2
  exit 1
fi
if docker exec "${FAULT_NODE}" test -d "${FAULT_POOL_PATH}/volumes/${RESERVATION_NAME}"; then
  echo "ENOSPC provisioning left a volume directory" >&2
  exit 1
fi

# Free the fault files without replacing the mounted filesystem. The retained
# reservation makes the next CreateVolume call idempotent on the same tmpfs.
docker exec "${FAULT_NODE}" sh -ec 'rm -f /srv/shiftpv-b/fill-*'
MOUNT_STATE=tmpfs_rw
FAULT_NODE_POD=$(kubectl -n shiftpv-system get pod \
  -l app.kubernetes.io/instance=shiftpv,app.kubernetes.io/component=node \
  --field-selector "spec.nodeName=${FAULT_NODE}" \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n shiftpv-system delete "pod/${FAULT_NODE_POD}" \
  --grace-period=0 --force --wait=true
kubectl -n shiftpv-system rollout status daemonset/shiftpv-node --timeout=5m
kubectl wait --for=condition=Ready pod/shiftpv-filesystem-fault --timeout=5m
kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/shiftpv-filesystem-fault --timeout=2m

FAULT_PV=$(kubectl get pvc shiftpv-filesystem-fault -o jsonpath='{.spec.volumeName}')
VOLUME_ID=$(kubectl get "pv/${FAULT_PV}" -o jsonpath='{.spec.csi.volumeHandle}')
if [[ "${VOLUME_ID}" != "${RESERVATION_NAME}" ]]; then
  echo "retry changed the reserved volume identity" >&2
  exit 1
fi
kubectl exec shiftpv-filesystem-fault -- grep -Fx 'ShiftPV filesystem fault recovery' /data/payload
docker exec "${FAULT_NODE}" test -f "${FAULT_POOL_PATH}/volumes/${VOLUME_ID}/payload"

# Stop the writer, remount the tmpfs filesystem read-only, and request deletion.
# DeleteVolume must fail retryably without dropping metadata or data, then
# finish after the filesystem is restored read-write.
kubectl delete pod shiftpv-filesystem-fault --wait=true
docker exec "${FAULT_NODE}" mount -o remount,ro "${FAULT_POOL_PATH}"
MOUNT_STATE=tmpfs_readonly
if docker exec "${FAULT_NODE}" touch "${FAULT_POOL_PATH}/.shiftpv-readonly-probe" 2>/dev/null; then
  echo "pool remount did not become read-only" >&2
  exit 1
fi

kubectl delete pvc shiftpv-filesystem-fault --wait=false
kubectl wait --for=delete pvc/shiftpv-filesystem-fault --timeout=2m
wait_for_unavailable_event PersistentVolume "${FAULT_PV}"
kubectl get "pv/${FAULT_PV}" >/dev/null
kubectl -n shiftpv-system get "configmap/${VOLUME_ID}" >/dev/null
docker exec "${FAULT_NODE}" test -f "${FAULT_POOL_PATH}/volumes/${VOLUME_ID}/payload"

docker exec "${FAULT_NODE}" mount -o remount,rw "${FAULT_POOL_PATH}"
MOUNT_STATE=tmpfs_rw
kubectl wait --for=delete "pv/${FAULT_PV}" --timeout=5m
kubectl -n shiftpv-system wait --for=delete "configmap/${VOLUME_ID}" --timeout=2m
docker exec "${FAULT_NODE}" test ! -e "${FAULT_POOL_PATH}/volumes/${VOLUME_ID}"
docker exec "${FAULT_NODE}" umount "${FAULT_POOL_PATH}"
MOUNT_STATE=normal
test ! -e "${WORKER_B_POOL}/volumes/${VOLUME_ID}"
kubectl delete storageclass shiftpv-filesystem-fault --wait=true

echo "ShiftPV filesystem fault recovery passed: volume=${VOLUME_ID} node=${FAULT_NODE}"
