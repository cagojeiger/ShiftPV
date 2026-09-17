#!/usr/bin/env bash
set -euo pipefail

# Proves the documented Retain reclaim path end to end: a `shiftpv-retain` PVC
# outlives its claim, the operator flips the released PV to `Delete`, and the
# node is left with no volume directory, no copy or placement marker and no
# lock. A leftover marker is exactly what makes the scanner report a Missing
# copy observation, so this scenario also asserts that signal stays silent.

: "${ROOT_DIR:=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)}"
# shellcheck source=test/e2e/kind/node-path.sh
source "${ROOT_DIR}/test/e2e/kind/node-path.sh"

: "${CLUSTER_NAME:?CLUSTER_NAME is required}"

NAMESPACE=shiftpv-retain-reclaim-e2e
NODE="${CLUSTER_NAME}-worker"
POOL=worker-a
POOL_PATH=/mnt/shiftpv
STORAGE_CLASS=shiftpv-retain
CLAIM=retained
PV_NAME=
VOLUME_ID=
COPY_ID=

cleanup() {
	local result_code=$?
	kubectl -n "${NAMESPACE}" delete pod "${CLAIM}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kubectl -n "${NAMESPACE}" delete pvc "${CLAIM}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	if [[ -n "${PV_NAME}" ]]; then
		kubectl patch "pv/${PV_NAME}" --type=merge \
			-p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}' >/dev/null 2>&1 || true
		kubectl delete "pv/${PV_NAME}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	fi
	kubectl delete namespace "${NAMESPACE}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	return "${result_code}"
}
trap cleanup EXIT

assert_retain_contract() {
	local policy binding
	policy=$(kubectl get "storageclass/${STORAGE_CLASS}" -o jsonpath='{.reclaimPolicy}')
	binding=$(kubectl get "storageclass/${STORAGE_CLASS}" -o jsonpath='{.volumeBindingMode}')
	if [[ "${policy}" != Retain || "${binding}" != WaitForFirstConsumer ]]; then
		echo "unexpected ${STORAGE_CLASS} contract: ${policy}/${binding}" >&2
		return 1
	fi
}

# A Retain PV survives its claim in Released. ShiftPV deliberately keeps the
# ShiftPVVolume and the on-node copy until the operator decides.
assert_released_volume_is_preserved() {
	kubectl wait --for=jsonpath='{.status.phase}'=Released "pv/${PV_NAME}" --timeout=2m >/dev/null
	kubectl get "shiftpvvolume/${VOLUME_ID}" >/dev/null
	assert_node_file "${NODE}" "${POOL_PATH}/volumes/${VOLUME_ID}/payload"
	assert_node_file "${NODE}" "${POOL_PATH}/.shiftpv/copy-${COPY_ID}.json"
	assert_node_file "${NODE}" "${POOL_PATH}/.shiftpv/placements/placement-${COPY_ID}.json"
}

assert_node_storage_reclaimed() {
	assert_node_absent "${NODE}" "${POOL_PATH}/volumes/${VOLUME_ID}"
	assert_node_absent "${NODE}" "${POOL_PATH}/.shiftpv/copy-${COPY_ID}.json"
	assert_node_absent "${NODE}" "${POOL_PATH}/.shiftpv/placements/placement-${COPY_ID}.json"
	assert_released_lock_holds_nothing
}

# `lock-<volume-id>` is the flock sentinel Store.Acquire opens, and unlinking a
# file while it is flocked would let the next opener take a different inode and
# believe it holds the same lock. The reclaim therefore leaves the sentinel
# behind on purpose; what it must not leave behind is state. Assert the file is
# empty and that nothing else in the control directory still names the volume or
# its copy.
assert_released_lock_holds_nothing() {
	local size residue
	size=$(docker exec "${NODE}" stat -c %s "${POOL_PATH}/.shiftpv/lock-${VOLUME_ID}" 2>/dev/null || echo missing)
	if [[ "${size}" != 0 ]]; then
		echo "volume lock sentinel is not an empty file: size=${size}" >&2
		return 1
	fi
	residue=$(docker exec "${NODE}" sh -c \
		"ls -A '${POOL_PATH}/.shiftpv' '${POOL_PATH}/.shiftpv/placements' '${POOL_PATH}/.shiftpv/retired' 2>/dev/null | grep -F -e '${VOLUME_ID}' -e '${COPY_ID}' || true")
	if [[ "${residue}" != "lock-${VOLUME_ID}" ]]; then
		echo "unexpected control-directory residue after reclaim: ${residue}" >&2
		return 1
	fi
}

# A fresh valid inventory that no longer names the volume and reports no absent
# copy is the report-only counterpart of shiftpv_copy_observations{state="Missing"}.
wait_for_clean_inventory() {
	local deadline=$((SECONDS + 120))
	while ((SECONDS < deadline)); do
		if kubectl get "shiftpvpool/${POOL}" -o json | jq -e --arg volume "${VOLUME_ID}" '
			.status.inventory.valid == true
			and all(.status.inventory.copies[]?; .identity.volumeID != $volume)
			and all(.status.inventory.copies[]?; .present == true and (.problem // "") == "")' >/dev/null; then
			return
		fi
		sleep 1
	done
	echo "Pool ${POOL} inventory did not settle without ${VOLUME_ID}" >&2
	kubectl get "shiftpvpool/${POOL}" -o yaml >&2 || true
	return 1
}

# Only meaningful in the group that also installs the metrics stack; elsewhere
# the inventory assertion above carries the same evidence.
assert_no_missing_copy_observation() {
	local result
	kubectl -n shiftpv-system get deployment/metrics-test >/dev/null 2>&1 || return 0
	result=$(kubectl -n shiftpv-system exec deployment/metrics-test -- \
		promtool query instant http://localhost:9090 \
		"sum(max_over_time(shiftpv_copy_observations{pool=\"${POOL}\",state=\"Missing\"}[5m]))")
	if [[ "${result}" != *"=> 0 "* ]]; then
		echo "shiftpv_copy_observations{state=\"Missing\"} was not zero across the reclaim: ${result}" >&2
		return 1
	fi
	printf '%s\n' "shiftpv_copy_observations Missing: ${result}"
}

for command in docker kubectl jq; do
	command -v "${command}" >/dev/null || {
		echo "required command not found: ${command}" >&2
		exit 1
	}
done

assert_retain_contract
kubectl create namespace "${NAMESPACE}"
kubectl -n "${NAMESPACE}" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${CLAIM}
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
  name: ${CLAIM}
spec:
  nodeSelector:
    kubernetes.io/hostname: ${NODE}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
    - name: writer
      image: busybox:1.37
      command: [sh, -ec]
      args:
        - printf 'ShiftPV retain reclaim\n' > /data/payload; sleep 3600
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${CLAIM}
EOF

kubectl -n "${NAMESPACE}" wait --for=condition=Ready "pod/${CLAIM}" --timeout=5m
kubectl -n "${NAMESPACE}" wait --for=jsonpath='{.status.phase}'=Bound "pvc/${CLAIM}" --timeout=2m
PV_NAME=$(kubectl -n "${NAMESPACE}" get "pvc/${CLAIM}" -o jsonpath='{.spec.volumeName}')
VOLUME_ID=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
COPY_ID=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.currentCopy.copyID}')
test -n "${COPY_ID}"
CHECKSUM=$(pod_sha256 "${NAMESPACE}" "${CLAIM}" /data/payload)
test "${CHECKSUM}" = "$(node_sha256 "${NODE}" "${POOL_PATH}/volumes/${VOLUME_ID}/payload")"

kubectl -n "${NAMESPACE}" delete "pod/${CLAIM}" --wait=true
kubectl -n "${NAMESPACE}" delete "pvc/${CLAIM}" --wait=true
assert_released_volume_is_preserved

# The documented reclaim: flip the checked PV to Delete and let CSI DeleteVolume
# run the durable cleanup contract.
kubectl patch "pv/${PV_NAME}" --type=merge \
	-p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}' >/dev/null
kubectl wait --for=delete "pv/${PV_NAME}" --timeout=5m
kubectl wait --for=delete "shiftpvvolume/${VOLUME_ID}" --timeout=2m
PV_NAME=
assert_node_storage_reclaimed
wait_for_clean_inventory
assert_no_missing_copy_observation

trap - EXIT
cleanup
echo "ShiftPV Retain volume reclaim E2E passed: volume=${VOLUME_ID} copy=${COPY_ID} checksum=${CHECKSUM}"
