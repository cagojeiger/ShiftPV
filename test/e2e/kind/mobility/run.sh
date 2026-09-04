#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)
CLUSTER_NAME=${CLUSTER_NAME:-shiftpv-mobility-e2e}
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0}
KEEP_CLUSTER=${KEEP_CLUSTER:-0}
PHASE_TIMEOUT_SECONDS=${PHASE_TIMEOUT_SECONDS:-180}
# shellcheck source=test/e2e/kind/mobility/recovery.sh
source "${ROOT_DIR}/test/e2e/kind/mobility/recovery.sh"
# shellcheck source=test/e2e/kind/mobility/preflight.sh
source "${ROOT_DIR}/test/e2e/kind/mobility/preflight.sh"

for command in docker kind kubectl helm sed; do
	command -v "${command}" >/dev/null || {
		echo "required command not found: ${command}" >&2
		exit 1
	}
done

mkdir -p "${ROOT_DIR}/.tmp"
WORK_DIR=$(mktemp -d "${ROOT_DIR}/.tmp/shiftpv-mobility.XXXXXX")
WORKER_A_POOL="${WORK_DIR}/worker-a"
WORKER_B_POOL="${WORK_DIR}/worker-b"
mkdir -p "${WORKER_A_POOL}" "${WORKER_B_POOL}"
export KUBECONFIG="${E2E_KUBECONFIG:-${WORK_DIR}/kubeconfig}"

cleanup() {
	if [[ "${KEEP_CLUSTER}" == "1" ]]; then
		echo "keeping cluster ${CLUSTER_NAME}, kubeconfig ${KUBECONFIG}, and data under ${WORK_DIR}"
		return
	fi
	kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
	rm -rf -- "${WORK_DIR}"
}
trap cleanup EXIT

assert_move_diagnostics() {
	local move=$1 expected_phase=$2 expected_reason=$3 expected_event=$4
	local phase reason message transition_time progress_time event_reasons="" deadline
	phase=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.phase}')
	reason=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.reason}')
	message=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.message}')
	transition_time=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.lastTransitionTime}')
	progress_time=$(kubectl get "shiftpvmove/${move}" -o jsonpath='{.status.lastProgressTime}')
	test "${phase}" = "${expected_phase}"
	test "${reason}" = "${expected_reason}"
	test -n "${message}"
	[[ "${transition_time}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T ]]
	[[ "${progress_time}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T ]]
	if [[ "${expected_phase}" == Blocked ]]; then
		grep -Fq 'operator action required' <<<"${message}"
	else
		grep -Eq 'automatic retry|no operator action is required' <<<"${message}"
	fi
	kubectl get "shiftpvmove/${move}" | sed -n '1p' | grep -q 'REASON.*LASTPROGRESS'
	deadline=$((SECONDS + 60))
	while ((SECONDS < deadline)); do
		event_reasons=$(kubectl -n default get events \
			--field-selector "involvedObject.kind=ShiftPVMove,involvedObject.name=${move}" \
			-o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null || true)
		grep -Fxq "${expected_event}" <<<"${event_reasons}" && return
		sleep 1
	done
	echo "missing ShiftPVMove event ${expected_event}; observed: ${event_reasons}" >&2
	return 1
}

sed \
	-e "s|__WORKER_A_POOL__|${WORKER_A_POOL}|g" \
	-e "s|__WORKER_B_POOL__|${WORKER_B_POOL}|g" \
	"${ROOT_DIR}/test/e2e/kind/cluster.yaml.tpl" >"${WORK_DIR}/cluster.yaml"
sed \
	-e "s|__WORKER_A_NODE__|${CLUSTER_NAME}-worker|g" \
	-e "s|__WORKER_B_NODE__|${CLUSTER_NAME}-worker2|g" \
	"${ROOT_DIR}/test/e2e/kind/pools.yaml.tpl" >"${WORK_DIR}/pools.yaml"

kind create cluster --name "${CLUSTER_NAME}" --image "${NODE_IMAGE}" --config "${WORK_DIR}/cluster.yaml"
docker build \
	--target combined \
	--build-arg CONTROLLER_VERSION=dev \
	--build-arg NODE_VERSION=dev \
	-f "${ROOT_DIR}/build/package/Dockerfile" \
	-t shiftpv:dev \
	"${ROOT_DIR}"
kind load docker-image shiftpv:dev --name "${CLUSTER_NAME}"

helm upgrade --install shiftpv "${ROOT_DIR}/charts/shiftpv" \
	--namespace shiftpv-system \
	--create-namespace \
	--values "${ROOT_DIR}/test/e2e/kind/values.yaml" \
	--set mobility.interval=10s \
	--wait \
	--timeout 5m
WEBHOOK_SECRET=shiftpv-webhook-tls
WEBHOOK_SERVICE=shiftpv-webhook
WEBHOOK_CONFIGURATION=shiftpv-mobility
for key in 'ca\.crt' 'ca\.key' 'tls\.crt' 'tls\.key'; do
	test -n "$(kubectl -n shiftpv-system get "secret/${WEBHOOK_SECRET}" -o "jsonpath={.data.${key}}")"
done
SERVICE_UID=$(kubectl -n shiftpv-system get service/shiftpv-webhook -o jsonpath='{.metadata.uid}')
DRIVER_UID=$(kubectl get csidriver/csi.shiftpv.io -o jsonpath='{.metadata.uid}')
test "$(kubectl -n shiftpv-system get "secret/${WEBHOOK_SECRET}" -o jsonpath='{.metadata.ownerReferences[0].kind}')" = "Service"
test "$(kubectl -n shiftpv-system get "secret/${WEBHOOK_SECRET}" -o jsonpath='{.metadata.ownerReferences[0].uid}')" = "${SERVICE_UID}"
test "$(kubectl get "mutatingwebhookconfiguration/${WEBHOOK_CONFIGURATION}" -o jsonpath='{.metadata.ownerReferences[0].kind}')" = "CSIDriver"
test "$(kubectl get "mutatingwebhookconfiguration/${WEBHOOK_CONFIGURATION}" -o jsonpath='{.metadata.ownerReferences[0].uid}')" = "${DRIVER_UID}"
WEBHOOK_CA=$(kubectl -n shiftpv-system get "secret/${WEBHOOK_SECRET}" -o jsonpath='{.data.ca\.crt}')
test "${WEBHOOK_CA}" = "$(kubectl get "mutatingwebhookconfiguration/${WEBHOOK_CONFIGURATION}" -o jsonpath='{.webhooks[0].clientConfig.caBundle}')"
WEBHOOK_CERT_BEFORE=$(kubectl -n shiftpv-system get "secret/${WEBHOOK_SECRET}" -o jsonpath='{.data.tls\.crt}')
kubectl apply -f "${WORK_DIR}/pools.yaml"
kubectl -n shiftpv-system wait --for=condition=Ready pod \
	-l app.kubernetes.io/instance=shiftpv --timeout=5m

BLOCKED_SOURCE_NODE="${CLUSTER_NAME}-worker"
test_preflight
# Real pre-commit copy failure retains the source recovery regression. The former
# source-only selector is now rejected non-disruptively by preflight.
kubectl cordon "${CLUSTER_NAME}-worker2"
sed \
	-e "s|__SOURCE_NODE__|${BLOCKED_SOURCE_NODE}|g" \
	-e '/      nodeSelector:/,+1d' \
	"${ROOT_DIR}/test/e2e/kind/mobility/manifests/blocked-workload.yaml.tpl" >"${WORK_DIR}/blocked-workload.yaml"
kubectl apply -f "${WORK_DIR}/blocked-workload.yaml"
kubectl -n shiftpv-mobility-blocked rollout status deployment/source-only --timeout=5m
kubectl -n shiftpv-mobility-blocked wait --for=jsonpath='{.status.phase}'=Bound pvc/source-only --timeout=2m
BLOCKED_PV=$(kubectl -n shiftpv-mobility-blocked get pvc source-only -o jsonpath='{.spec.volumeName}')
BLOCKED_VOLUME=$(kubectl get "pv/${BLOCKED_PV}" -o jsonpath='{.spec.csi.volumeHandle}')
kubectl uncordon "${CLUSTER_NAME}-worker2"
kubectl cordon "${BLOCKED_SOURCE_NODE}"
for _ in {1..300}; do
	BLOCKED_MOVE=$(kubectl get shiftpvmoves -o jsonpath="{.items[?(@.spec.volumeID=='${BLOCKED_VOLUME}')].metadata.name}" 2>/dev/null || true)
	if [[ -n "${BLOCKED_MOVE}" ]]; then
		if [[ -z "${COPY_FAULT_PATH:-}" ]]; then
			COPY_FAULT_PATH="${WORKER_B_POOL}/.shiftpv/incoming/${BLOCKED_MOVE}"
			mkdir -p "${WORKER_B_POOL}/.shiftpv/incoming"
			test ! -e "${COPY_FAULT_PATH}"
			touch "${COPY_FAULT_PATH}"
		fi
		BLOCKED_PHASE=$(kubectl get "shiftpvmove/${BLOCKED_MOVE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		[[ "${BLOCKED_PHASE}" == "Blocked" ]] && break
	fi
	sleep 1
done
test "${BLOCKED_PHASE:-}" = "Blocked"
test "$(kubectl get "shiftpvmove/${BLOCKED_MOVE}" -o jsonpath='{.status.reason}')" = "CopyFailed"
assert_move_diagnostics "${BLOCKED_MOVE}" Blocked CopyFailed CopyFailed
test "$(kubectl get "shiftpvvolume/${BLOCKED_VOLUME}" -o jsonpath='{.status.phase}')" = "Blocked"
test "$(kubectl get "shiftpvvolume/${BLOCKED_VOLUME}" -o jsonpath='{.status.ownerNode}')" = "${BLOCKED_SOURCE_NODE}"
test -f "${WORKER_A_POOL}/volumes/${BLOCKED_VOLUME}/payload"
test ! -e "${WORKER_B_POOL}/volumes/${BLOCKED_VOLUME}"
kubectl uncordon "${BLOCKED_SOURCE_NODE}"
rm -- "${COPY_FAULT_PATH}"

echo "ShiftPV blocked mobility E2E passed: volume=${BLOCKED_VOLUME} move=${BLOCKED_MOVE} reason=CopyFailed"
recover_source_only

kubectl apply -f "${ROOT_DIR}/test/e2e/kind/mobility/manifests/wffc-workload.yaml"
kubectl -n shiftpv-mobility-test rollout status deployment/wffc --timeout=5m
kubectl -n shiftpv-mobility-test wait --for=jsonpath='{.status.phase}'=Bound pvc/wffc --timeout=2m
# Recreate once on the owner so admission injects a hostname pin. That pin must
# not be mistaken for a user constraint during the subsequent normal migration.
kubectl -n shiftpv-mobility-test delete pod -l app=shiftpv-mobility-wffc --wait=true --timeout=120s
kubectl -n shiftpv-mobility-test rollout status deployment/wffc --timeout=180s
test "$(kubectl -n shiftpv-mobility-test get pod -l app=shiftpv-mobility-wffc -o jsonpath='{.items[0].metadata.annotations.shiftpv\.io/placement}')" = owner

PVC_UID=$(kubectl -n shiftpv-mobility-test get pvc wffc -o jsonpath='{.metadata.uid}')
PV_NAME=$(kubectl -n shiftpv-mobility-test get pvc wffc -o jsonpath='{.spec.volumeName}')
VOLUME_ID=$(kubectl get "pv/${PV_NAME}" -o jsonpath='{.spec.csi.volumeHandle}')
OLD_POD=$(kubectl -n shiftpv-mobility-test get pod -l app=shiftpv-mobility-wffc -o jsonpath='{.items[0].metadata.name}')
OLD_POD_UID=$(kubectl -n shiftpv-mobility-test get pod "${OLD_POD}" -o jsonpath='{.metadata.uid}')
SOURCE_NODE=$(kubectl -n shiftpv-mobility-test get pod "${OLD_POD}" -o jsonpath='{.spec.nodeName}')
CHECKSUM_BEFORE=$(kubectl -n shiftpv-mobility-test exec "${OLD_POD}" -- sha256sum /data/payload | awk '{print $1}')

if [[ "${SOURCE_NODE}" == "${CLUSTER_NAME}-worker" ]]; then
	DESTINATION_NODE="${CLUSTER_NAME}-worker2"
else
	DESTINATION_NODE="${CLUSTER_NAME}-worker"
fi

kubectl cordon "${SOURCE_NODE}"
for _ in {1..120}; do
	MOVE_NAME=$(kubectl get shiftpvmoves -o jsonpath="{.items[?(@.spec.volumeID=='${VOLUME_ID}')].metadata.name}" 2>/dev/null || true)
	[[ -n "${MOVE_NAME}" ]] && break
	sleep 1
done
test -n "${MOVE_NAME}"

if kubectl patch "shiftpvmove/${MOVE_NAME}" --type merge -p '{"spec":{"recovery":"ResumeOwner"}}' >"${WORK_DIR}/early-recovery.txt" 2>&1; then
	echo 'recovery was accepted before the move was Blocked' >&2
	exit 1
fi
grep -q 'recovery may only be requested on a Blocked move' "${WORK_DIR}/early-recovery.txt"

restart_at_phase() {
	local phase=$1
	local deadline=$((SECONDS + PHASE_TIMEOUT_SECONDS))
	local current=""
	while ((SECONDS < deadline)); do
		current=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
		if [[ "${current}" == "${phase}" ]]; then
			if [[ "${phase}" == "Committing" ]]; then
				local committed_owner
				committed_owner=$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}' 2>/dev/null || true)
				if [[ "${committed_owner}" != "${DESTINATION_NODE}" ]]; then
					sleep 0.1
					continue
				fi
			fi
			local pod uid_before uid_after
			pod=$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}')
			uid_before=$(kubectl -n shiftpv-system get "pod/${pod}" -o jsonpath='{.metadata.uid}')
			kubectl -n shiftpv-system delete "pod/${pod}" --grace-period=0 --force --wait=true
			kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=5m
			uid_after=$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.uid}')
			test "${uid_before}" != "${uid_after}"
			echo "controller restart injected at ${phase}: ${uid_before} -> ${uid_after}"
			return
		fi
		[[ "${current}" == "Blocked" || "${current}" == "Succeeded" ]] && return 1
		sleep 0.2
	done
	echo "phase ${phase} was not observed within ${PHASE_TIMEOUT_SECONDS}s; current=${current}" >&2
	kubectl get "shiftpvmove/${MOVE_NAME}" -o yaml >&2 || true
	return 1
}

restart_at_phase Copying
restart_at_phase Promoting
restart_at_phase Committing

for _ in {1..600}; do
	MOVE_PHASE=$(kubectl get "shiftpvmove/${MOVE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
	[[ "${MOVE_PHASE}" == "Succeeded" || "${MOVE_PHASE}" == "Blocked" ]] && break
	sleep 1
done
if [[ "${MOVE_PHASE}" != "Succeeded" ]]; then
	kubectl get "shiftpvmove/${MOVE_NAME}" -o yaml >&2
	exit 1
fi
assert_move_diagnostics "${MOVE_NAME}" Succeeded '' MobilitySucceeded

kubectl -n shiftpv-mobility-test rollout status deployment/wffc --timeout=5m
NEW_POD=$(kubectl -n shiftpv-mobility-test get pod -l app=shiftpv-mobility-wffc -o jsonpath='{.items[0].metadata.name}')
NEW_POD_UID=$(kubectl -n shiftpv-mobility-test get pod "${NEW_POD}" -o jsonpath='{.metadata.uid}')
test "${NEW_POD_UID}" != "${OLD_POD_UID}"
test "$(kubectl -n shiftpv-mobility-test get pod "${NEW_POD}" -o jsonpath='{.spec.nodeName}')" = "${DESTINATION_NODE}"

CHECKSUM_AFTER=$(kubectl -n shiftpv-mobility-test exec "${NEW_POD}" -- sha256sum /data/payload | awk '{print $1}')
test "${CHECKSUM_BEFORE}" = "${CHECKSUM_AFTER}"
test "$(kubectl -n shiftpv-mobility-test get pvc wffc -o jsonpath='{.metadata.uid}')" = "${PVC_UID}"
test "$(kubectl -n shiftpv-mobility-test get pvc wffc -o jsonpath='{.spec.volumeName}')" = "${PV_NAME}"
test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.phase}')" = "Ready"
test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.ownerNode}')" = "${DESTINATION_NODE}"
test "$(kubectl get "shiftpvvolume/${VOLUME_ID}" -o jsonpath='{.status.activeMove}')" = ""
if [[ "${DESTINATION_NODE}" == "${CLUSTER_NAME}-worker" ]]; then
	DESTINATION_POOL="${WORKER_A_POOL}"
	SOURCE_POOL="${WORKER_B_POOL}"
else
	DESTINATION_POOL="${WORKER_B_POOL}"
	SOURCE_POOL="${WORKER_A_POOL}"
fi
test -f "${DESTINATION_POOL}/volumes/${VOLUME_ID}/payload"
test -f "${SOURCE_POOL}/.shiftpv/retired/${MOVE_NAME}/payload"
test "${WEBHOOK_CERT_BEFORE}" = "$(kubectl -n shiftpv-system get "secret/${WEBHOOK_SECRET}" -o jsonpath='{.data.tls\.crt}')"

recover_after_commit_failure
assert_retained_volumes_unchanged

helm upgrade shiftpv "${ROOT_DIR}/charts/shiftpv" \
	--namespace shiftpv-system \
	--reuse-values \
	--set mobility.enabled=false \
	--wait \
	--timeout 5m
kubectl -n shiftpv-system get "service/${WEBHOOK_SERVICE}" >/dev/null
kubectl -n shiftpv-system get "secret/${WEBHOOK_SECRET}" >/dev/null
test "$(kubectl get "mutatingwebhookconfiguration/${WEBHOOK_CONFIGURATION}" -o jsonpath='{.webhooks[0].failurePolicy}')" = "Ignore"
test "$(kubectl get "mutatingwebhookconfiguration/${WEBHOOK_CONFIGURATION}" -o jsonpath='{.webhooks[0].matchConditions[0].expression}')" = "false"
test "${WEBHOOK_CERT_BEFORE}" = "$(kubectl -n shiftpv-system get "secret/${WEBHOOK_SECRET}" -o jsonpath='{.data.tls\.crt}')"

echo "ShiftPV closed-loop mobility E2E passed"
echo "volume=${VOLUME_ID} pv=${PV_NAME} move=${MOVE_NAME} source=${SOURCE_NODE} destination=${DESTINATION_NODE} checksum=${CHECKSUM_AFTER}; webhook certificates reconciled and disabled admission is inert"
