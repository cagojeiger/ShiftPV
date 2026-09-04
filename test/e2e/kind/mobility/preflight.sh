#!/usr/bin/env bash
# Sourced by run.sh. All resources belong to its isolated Kind cluster.
PREFLIGHT_RETAINED_VOLUMES=()
PREFLIGHT_EXPECTED_MOVES=()

assert_retained_volumes_unchanged() {
	local i volume moves
	for i in "${!PREFLIGHT_RETAINED_VOLUMES[@]}"; do
		volume=${PREFLIGHT_RETAINED_VOLUMES[i]}
		moves=$(kubectl get shiftpvmoves -o jsonpath="{.items[?(@.spec.volumeID=='${volume}')].metadata.name}")
		test "${moves}" = "${PREFLIGHT_EXPECTED_MOVES[i]}"
		test "$(kubectl get "shiftpvvolume/${volume}" -o jsonpath='{.status.phase}')" = Ready
		test -z "$(kubectl get "shiftpvvolume/${volume}" -o jsonpath='{.status.activeMove}')"
	done
}

assert_preflight_preserves_consumer() {
	local scenario=$1 pod=$2 uid=$3 volume=$4 checksum=$5
	local deadline=$((SECONDS + 25))
	while ((SECONDS < deadline)); do
		test "$(kubectl -n shiftpv-mobility-blocked get "pod/${pod}" -o jsonpath='{.metadata.uid}')" = "${uid}"
		test -z "$(kubectl -n shiftpv-mobility-blocked get "pod/${pod}" -o jsonpath='{.metadata.deletionTimestamp}')"
		test "$(kubectl get "shiftpvvolume/${volume}" -o jsonpath='{.status.phase}')" = Ready
		test -z "$(kubectl get "shiftpvvolume/${volume}" -o jsonpath='{.status.activeMove}')"
		test -z "$(kubectl get shiftpvmoves -o jsonpath="{.items[?(@.spec.volumeID=='${volume}')].metadata.name}")"
		sleep 2
	done
	test "$(kubectl -n shiftpv-mobility-blocked exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')" = "${checksum}"
	kubectl -n shiftpv-mobility-blocked exec "${pod}" -- sh -ec 'printf "preflight still writable\n" > /data/probe; test -s /data/probe'
	echo "preflight ${scenario} passed: same Pod UID=${uid}, Ready volume=${volume}, no move, data writable"
}

assert_obsolete_pending_move_cancelled() {
	local volume=$1 source=$2 move deadline
	move="obsolete-${volume#shiftpv-}"
	deadline=$((SECONDS + 60))
	kubectl create -f - <<EOF
apiVersion: shiftpv.io/v1alpha1
kind: ShiftPVMove
metadata:
  name: ${move}
spec:
  volumeID: ${volume}
  sourceNode: ${source}
EOF
	while ((SECONDS < deadline)); do
		if ! kubectl get "shiftpvmove/${move}" >/dev/null 2>&1; then
			assert_retained_volumes_unchanged
			echo "obsolete pre-lock Move cancellation passed: volume=${volume} source=${source}"
			return
		fi
		sleep 1
	done
	echo "obsolete pre-lock Move was not cancelled: ${move}" >&2
	return 1
}

test_preflight() {
	local scenario pod uid pv volume checksum patch controller_pod move
	for scenario in selector affinity taint pdb; do
		move=""
		# Choose the initial owner without persisting a workload node pin.
		kubectl cordon "${CLUSTER_NAME}-worker2"
		sed -e "s|__SOURCE_NODE__|${CLUSTER_NAME}-worker|g" \
			-e '/      nodeSelector:/,+1d' -e 's/replicas: 1/replicas: 0/' \
			"${ROOT_DIR}/test/e2e/kind/mobility/manifests/blocked-workload.yaml.tpl" >"${WORK_DIR}/preflight-${scenario}.yaml"
		kubectl apply -f "${WORK_DIR}/preflight-${scenario}.yaml"
		case "${scenario}" in
		selector)
			patch="{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"${CLUSTER_NAME}-worker\"}}}}}"
			kubectl -n shiftpv-mobility-blocked patch deployment/source-only --type merge -p "${patch}"
			;;
		affinity)
			patch="{\"spec\":{\"template\":{\"spec\":{\"affinity\":{\"nodeAffinity\":{\"requiredDuringSchedulingIgnoredDuringExecution\":{\"nodeSelectorTerms\":[{\"matchExpressions\":[{\"key\":\"kubernetes.io/hostname\",\"operator\":\"In\",\"values\":[\"${CLUSTER_NAME}-worker\"]}]}]}}}}}}}"
			kubectl -n shiftpv-mobility-blocked patch deployment/source-only --type merge -p "${patch}"
			;;
		taint) kubectl taint node "${CLUSTER_NAME}-worker2" shiftpv-e2e=blocked:NoSchedule ;;
		pdb) kubectl -n shiftpv-mobility-blocked create poddisruptionbudget keep-source --selector=app=shiftpv-mobility-source-only --min-available=1 ;;
		esac
		kubectl -n shiftpv-mobility-blocked scale deployment/source-only --replicas=1
		kubectl -n shiftpv-mobility-blocked rollout status deployment/source-only --timeout=180s
		pod=$(kubectl -n shiftpv-mobility-blocked get pod -l app=shiftpv-mobility-source-only -o jsonpath='{.items[0].metadata.name}')
		uid=$(kubectl -n shiftpv-mobility-blocked get "pod/${pod}" -o jsonpath='{.metadata.uid}')
		pv=$(kubectl -n shiftpv-mobility-blocked get pvc/source-only -o jsonpath='{.spec.volumeName}')
		volume=$(kubectl get "pv/${pv}" -o jsonpath='{.spec.csi.volumeHandle}')
		checksum=$(kubectl -n shiftpv-mobility-blocked exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')
		kubectl uncordon "${CLUSTER_NAME}-worker2"
		kubectl cordon "${CLUSTER_NAME}-worker"
		assert_preflight_preserves_consumer "${scenario}" "${pod}" "${uid}" "${volume}" "${checksum}"
		assert_retained_volumes_unchanged
		if [[ "${scenario}" == selector ]]; then
			controller_pod=$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}')
			kubectl -n shiftpv-system delete "pod/${controller_pod}" --wait=true --timeout=120s
			kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=180s
			assert_preflight_preserves_consumer selector-restart "${pod}" "${uid}" "${volume}" "${checksum}"
		fi
		# PDB removal must allow the same previously deferred volume to move.
		if [[ "${scenario}" == pdb ]]; then
			kubectl -n shiftpv-mobility-blocked delete pdb/keep-source
			for _ in {1..90}; do
				move=$(kubectl get shiftpvmoves -o jsonpath="{.items[?(@.spec.volumeID=='${volume}')].metadata.name}")
				[[ -n "${move}" ]] && break
				sleep 1
			done
			test -n "${move}"
			kubectl wait "shiftpvmove/${move}" --for=jsonpath='{.status.phase}'=Succeeded --timeout=480s
			assert_move_diagnostics "${move}" Succeeded '' MobilitySucceeded
			kubectl -n shiftpv-mobility-blocked rollout status deployment/source-only --timeout=180s
			pod=$(kubectl -n shiftpv-mobility-blocked get pod -l app=shiftpv-mobility-source-only -o jsonpath='{.items[0].metadata.name}')
			test "$(kubectl -n shiftpv-mobility-blocked exec "${pod}" -- sha256sum /data/payload | awk '{print $1}')" = "${checksum}"
			test "$(kubectl get "shiftpvvolume/${volume}" -o jsonpath='{.status.ownerNode}')" = "${CLUSTER_NAME}-worker2"
			echo 'preflight PDB removal passed: deferred volume migrated automatically with same data'
		fi
		kubectl uncordon "${CLUSTER_NAME}-worker"
		if [[ "${scenario}" == taint ]]; then kubectl taint node "${CLUSTER_NAME}-worker2" shiftpv-e2e:NoSchedule-; fi
		kubectl delete namespace shiftpv-mobility-blocked --wait=true --timeout=180s
		PREFLIGHT_RETAINED_VOLUMES+=("${volume}")
		PREFLIGHT_EXPECTED_MOVES+=("${move}")
		assert_retained_volumes_unchanged
		if [[ "${scenario}" == taint ]]; then
			assert_obsolete_pending_move_cancelled "${volume}" "${CLUSTER_NAME}-worker"
		fi
	done
}
