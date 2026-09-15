#!/usr/bin/env bash

# Quiesce and resume the controller so a fault can be injected at an exact
# transaction boundary. controller_down only returns once no controller Pod
# remains, so the caller never races an overlapping reconcile.
controller_down() {
	kubectl -n shiftpv-system scale deployment/shiftpv-controller --replicas=0
	local deadline=$((SECONDS + 120))
	while ((SECONDS < deadline)); do
		if [[ -z "$(kubectl -n shiftpv-system get pod -l app.kubernetes.io/component=controller -o name 2>/dev/null)" ]]; then
			return
		fi
		sleep 1
	done
	echo 'controller Pod did not stop' >&2
	return 1
}

controller_up() {
	kubectl -n shiftpv-system scale deployment/shiftpv-controller --replicas=1
	kubectl -n shiftpv-system rollout status deployment/shiftpv-controller --timeout=180s
}
