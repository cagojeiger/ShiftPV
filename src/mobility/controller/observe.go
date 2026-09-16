package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
	"github.com/project-jelly/ShiftPV/src/mobility/admission"
	"github.com/project-jelly/ShiftPV/src/mobility/fsm"
	"github.com/project-jelly/ShiftPV/src/volume"
)

type observation struct {
	FSM             fsm.Observation
	Volume          volumeapi.State
	VolumeMissing   bool
	PV              *corev1.PersistentVolume
	Claim           *corev1.PersistentVolumeClaim
	Consumer        *corev1.Pod
	Replacement     *corev1.Pod
	Placement       *corev1.Pod
	DestinationNode string
	CandidateNodes  []string
	Names           resourceNames
	SourceCordoned  bool
	// PendingObsolete marks an unstarted Move whose source is schedulable again.
	// The reconciler deletes the transaction instead of deciding a next phase.
	PendingObsolete bool
}

// poolIndex is the Pool half of one observation snapshot, indexed by node name.
// registered holds every validated ShiftPVPool; ready holds only those whose
// inventory is currently publishable.
type poolIndex struct {
	registered map[string]volumeapi.Pool
	ready      map[string]volumeapi.Pool
}

// observe builds the immutable snapshot the reconciler decides on. Each step
// below either fills part of the snapshot or reports done, meaning the partial
// snapshot it has already filled is the final diagnosis.
func (r *Reconciler) observe(ctx context.Context, move volumeapi.Move) (observation, error) {
	result := observation{Names: namesFor(move.Name)}
	pools, done, err := r.observeEligibility(ctx, move, &result)
	if done || err != nil {
		return result, err
	}
	observeMoveState(move, &result)
	observeReplacement(move, &result)
	if err := r.observePlacement(ctx, move, &result); err != nil {
		return result, err
	}
	repaired, err := r.observeDestination(ctx, move, pools, &result)
	if err != nil {
		return result, err
	}
	if err := r.observeJobs(ctx, move, repaired, &result); err != nil {
		return result, err
	}
	observeDestinationPublish(move, pools, &result)
	// Evaluate the cleanup step before the return operands are read: the Go
	// spec leaves the order between a plain operand and a call unspecified.
	if err := r.observeCleanup(ctx, move, &result); err != nil {
		return result, err
	}
	return result, nil
}

// preflightVolume judges whether a volume with no transaction yet may safely get
// one, which is all discovery asks. It returns only the verdict: every flag that
// observe derives from a Move's persisted status is meaningless here, and every
// Move-scoped child resource — the placement reservation, the transfer Jobs, the
// cleanup journal — is named after a Move that does not exist.
func (r *Reconciler) preflightVolume(ctx context.Context, volumeID, sourceNode string) (valid bool, reason string, err error) {
	var result observation
	candidate := volumeapi.Move{Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: sourceNode}}
	if _, _, err := r.observeEligibility(ctx, candidate, &result); err != nil {
		return false, "", err
	}
	return result.FSM.PreconditionsValid, result.FSM.UnsafeReason, nil
}

// observeEligibility reads the facts that do not depend on transaction progress:
// volume authority, Pools, source health, destination candidates, PV/PVC
// binding, consumers and the preflight verdict. It returns the Pool snapshot its
// later siblings reuse so readiness is read once per observation.
func (r *Reconciler) observeEligibility(ctx context.Context, move volumeapi.Move, result *observation) (poolIndex, bool, error) {
	if done, err := r.observeVolume(ctx, move, result); done || err != nil {
		return poolIndex{}, true, err
	}
	pools, err := r.observePools(ctx)
	if err != nil {
		return pools, true, err
	}
	if done, err := r.observeSource(ctx, move, pools, result); done || err != nil {
		return pools, true, err
	}
	if err := r.observeCandidates(ctx, move, pools, result); err != nil {
		return pools, true, err
	}
	if done, err := r.observeBinding(ctx, move, result); done || err != nil {
		return pools, true, err
	}
	if done, err := r.observeConsumers(ctx, move, result); done || err != nil {
		return pools, true, err
	}
	if err := r.observePreconditions(ctx, move, result); err != nil {
		return pools, true, err
	}
	return pools, false, nil
}

func (r *Reconciler) observeVolume(ctx context.Context, move volumeapi.Move, result *observation) (bool, error) {
	state, err := r.Repository.Get(ctx, move.Spec.VolumeID)
	if err != nil {
		if apierrors.IsNotFound(err) && completionAllowed(move, state, true) {
			result.VolumeMissing = true
			result.FSM.CompletionReady = true
			return true, nil
		}
		return false, err
	}
	result.Volume = state
	result.DestinationNode = move.Status.DestinationNode
	// Completing is persisted cleanup evidence. Finalization reads authority only;
	// expired Jobs or a deleted PVC must not restart disk work after unlock.
	if move.Status.Phase == string(fsm.PhaseCompleting) {
		result.FSM.CompletionReady = completionAllowed(move, state, false)
		return true, nil
	}
	result.FSM.OwnerCommitted = hasCommittedDestinationAuthority(move, state, false)
	return false, nil
}

func (r *Reconciler) observePools(ctx context.Context) (poolIndex, error) {
	var index poolIndex
	pools, err := r.Repository.Pools(ctx)
	if err != nil {
		return index, err
	}
	index.registered = make(map[string]volumeapi.Pool, len(pools))
	for _, pool := range pools {
		if pool.NodeName == "" || !filepath.IsAbs(pool.MountPath) || filepath.Clean(pool.MountPath) == "/" {
			return index, fmt.Errorf("ShiftPVPool %q has invalid nodeName or mountPath", pool.Name)
		}
		if _, duplicate := index.registered[pool.NodeName]; duplicate {
			return index, fmt.Errorf("multiple ShiftPVPools are registered for node %q", pool.NodeName)
		}
		index.registered[pool.NodeName] = pool
	}
	readyPools, err := r.Repository.ReadyPools(ctx)
	if err != nil {
		return index, err
	}
	index.ready = make(map[string]volumeapi.Pool, len(readyPools))
	for _, pool := range readyPools {
		index.ready[pool.NodeName] = pool
	}
	return index, nil
}

func (r *Reconciler) observeSource(ctx context.Context, move volumeapi.Move, pools poolIndex, result *observation) (bool, error) {
	state := result.Volume
	sourceNode, err := r.Client.CoreV1().Nodes().Get(ctx, move.Spec.SourceNode, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("read source Node: %w", err)
	}
	sourcePool, sourceReady := pools.ready[move.Spec.SourceNode]
	sourceHealthy := err == nil && admission.NodeReady(sourceNode) && sourceReady && sourceCopyPresent(sourcePool, state.CurrentCopy)
	result.FSM.SourceHealthy = sourceHealthy
	result.SourceCordoned = sourceNode != nil && sourceNode.Spec.Unschedulable
	if !sourceHealthy && !result.FSM.OwnerCommitted {
		result.FSM.UnsafeReason = "SourceUnavailable"
	}
	// Discovery and Node updates are not atomic. A Move may be created from a
	// cordoned snapshot just after the source was uncordoned. Before any volume
	// lock or helper action, prefer the current Node observation and let the
	// reconciler remove that obsolete transaction even if its PVC is disappearing.
	result.PendingObsolete = move.Status.Phase == string(fsm.PhasePending) && sourceHealthy && !result.SourceCordoned &&
		state.Phase == volumeapi.PhaseReady && state.ActiveMove == "" && state.OwnerNode == move.Spec.SourceNode
	if result.PendingObsolete {
		result.FSM.PreflightDeferred = true
		result.FSM.UnsafeReason = "SourceNotCordoned"
		return true, nil
	}
	return false, nil
}

func (r *Reconciler) observeCandidates(ctx context.Context, move volumeapi.Move, pools poolIndex, result *observation) error {
	for nodeName := range pools.registered {
		if nodeName == move.Spec.SourceNode {
			continue
		}
		readyPool, ready := pools.ready[nodeName]
		if !ready || volumeapi.PoolHasConflictingServingVolume(readyPool, move.Spec.VolumeID, move.Status.DestinationCopy) {
			continue
		}
		node, nodeErr := r.Client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if nodeErr != nil {
			if apierrors.IsNotFound(nodeErr) {
				continue
			}
			return fmt.Errorf("read destination Node %q: %w", nodeName, nodeErr)
		}
		if admission.NodeReady(node) && !node.Spec.Unschedulable {
			result.CandidateNodes = append(result.CandidateNodes, nodeName)
		}
	}
	if len(move.Status.CandidateNodes) != 0 {
		// CandidateNodes is the immutable eligibility snapshot taken before
		// eviction. Keep it stable so a transient Pool outage after placement
		// pauses the transaction instead of being misclassified as a changed
		// scheduling constraint. The selected destination is checked against
		// current readiness later before any disk or authority action proceeds.
		result.CandidateNodes = append([]string(nil), move.Status.CandidateNodes...)
	}
	return nil
}

// observeBinding resolves the PV/PVC pair this volume is still bound to and the
// namespace opt-in that makes its workload movable at all.
func (r *Reconciler) observeBinding(ctx context.Context, move volumeapi.Move, result *observation) (bool, error) {
	persistentVolumes, err := r.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list PersistentVolumes: %w", err)
	}
	for index := range persistentVolumes.Items {
		candidate := &persistentVolumes.Items[index]
		if candidate.Spec.CSI != nil && candidate.Spec.CSI.Driver == admission.DriverName && candidate.Spec.CSI.VolumeHandle == move.Spec.VolumeID {
			result.PV = candidate.DeepCopy()
			break
		}
	}
	if result.PV == nil || result.PV.Spec.ClaimRef == nil {
		result.FSM.UnsafeReason = "VolumeBindingMissing"
		result.FSM.SourceAuthorityInvalid = true
		return true, nil
	}
	claimRef := result.PV.Spec.ClaimRef
	claim, err := r.Client.CoreV1().PersistentVolumeClaims(claimRef.Namespace).Get(ctx, claimRef.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			result.FSM.UnsafeReason = "VolumeBindingMissing"
			result.FSM.SourceAuthorityInvalid = true
			return true, nil
		}
		return false, fmt.Errorf("read PVC: %w", err)
	}
	result.Claim = claim
	// Names can be reused after PVC/namespace deletion while Retain PVs and
	// ShiftPVVolumes survive. Never associate that old volume with the new Pod.
	if !validBinding(result.PV, claim, move.Spec.VolumeID) {
		result.FSM.UnsafeReason = "VolumeBindingMismatch"
		result.FSM.SourceAuthorityInvalid = true
		return true, nil
	}
	if result.Volume.OwnerNode != move.Spec.SourceNode && !result.FSM.OwnerCommitted {
		result.FSM.UnsafeReason = "OwnerMismatch"
		result.FSM.SourceAuthorityInvalid = true
		return true, nil
	}
	namespace, err := r.Client.CoreV1().Namespaces().Get(ctx, claim.Namespace, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("read workload Namespace: %w", err)
	}
	if namespace.Labels[admissionNamespaceLabel] != "enabled" {
		result.FSM.UnsafeReason = "AdmissionNotEnabled"
		result.FSM.PreflightDeferred = preEviction(move)
		return true, nil
	}
	return false, nil
}

// observeConsumers separates the Pod this transaction is moving from any other
// live Pod on the claim, which after eviction is the replacement workload.
func (r *Reconciler) observeConsumers(ctx context.Context, move volumeapi.Move, result *observation) (bool, error) {
	claim := result.Claim
	pods, err := r.Client.CoreV1().Pods(claim.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list consumer Pods: %w", err)
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if !podUsesClaim(pod, claim.Name) || terminalPod(pod) {
			continue
		}
		if move.Status.ConsumerName != "" && pod.Name == move.Status.ConsumerName &&
			(move.Status.ConsumerUID == "" || string(pod.UID) == move.Status.ConsumerUID) {
			result.Consumer = pod.DeepCopy()
			continue
		}
		if move.Status.ConsumerName == "" && pod.Spec.NodeName == move.Spec.SourceNode {
			if result.Consumer != nil {
				result.FSM.UnsafeReason = "MultipleConsumers"
				result.FSM.PreflightDeferred = preEviction(move)
				return true, nil
			}
			result.Consumer = pod.DeepCopy()
			continue
		}
		if result.Replacement == nil {
			result.Replacement = pod.DeepCopy()
		}
	}
	return false, nil
}

func (r *Reconciler) observePreconditions(ctx context.Context, move volumeapi.Move, result *observation) error {
	sourceHealthy := result.FSM.SourceHealthy
	sourceCordoned := result.SourceCordoned
	preconditions := sourceHealthy && !result.FSM.SourceAuthorityInvalid && sourceCordoned && len(result.CandidateNodes) > 0 && result.Consumer != nil && metav1.GetControllerOf(result.Consumer) != nil
	// The original consumer is expected to disappear after eviction. Eligibility
	// diagnostics must not mask CopyFailed/PromotionFailed/CleanupFailed later.
	preflight := move.Status.Phase == "" || move.Status.Phase == string(fsm.PhasePending)
	if preflight && !preconditions && result.FSM.UnsafeReason == "" {
		if reason := unmetPreconditionReason(result, sourceCordoned); reason != "" {
			result.FSM.UnsafeReason = reason
		}
	}
	result.FSM.PreconditionsValid = preconditions
	if preEviction(move) && result.Consumer != nil && sourceHealthy {
		return r.deferOnPreflight(ctx, result, preconditions)
	}
	return nil
}

// unmetPreconditionReason names the first unmet eligibility precondition, or
// "" when none of the diagnosable ones is the cause.
func unmetPreconditionReason(result *observation, sourceCordoned bool) string {
	switch {
	case result.Consumer == nil:
		return "ControlledConsumerMissing"
	case metav1.GetControllerOf(result.Consumer) == nil:
		return "BarePodUnsupported"
	case len(result.CandidateNodes) == 0:
		return "DestinationUnavailable"
	case !sourceCordoned:
		return "SourceNotCordoned"
	}
	return ""
}

// deferOnPreflight runs preflight on a transaction that has not evicted yet and
// records the deferral it reports. A failing precondition with no preflight
// reason of its own still defers, under the eligibility reason already
// diagnosed or the generic one.
func (r *Reconciler) deferOnPreflight(ctx context.Context, result *observation, preconditions bool) error {
	reason, err := r.preflight(ctx, result)
	if err != nil {
		return err
	}
	if reason == "" && !preconditions {
		reason = result.FSM.UnsafeReason
		if reason == "" {
			reason = "PreconditionFailed"
		}
	}
	if reason != "" {
		result.FSM.PreconditionsValid = false
		result.FSM.PreflightDeferred = true
		result.FSM.UnsafeReason = reason
	}
	return nil
}

// observeMoveState projects the persisted Move status and the volume lock onto
// the FSM flags. It reads no API and adds no diagnosis beyond the capacity
// reason the capacity reconciler already recorded.
func observeMoveState(move volumeapi.Move, result *observation) {
	state := result.Volume
	result.FSM.VolumeLocked = state.Phase == volumeapi.PhaseMoving && state.ActiveMove == move.Name && state.OwnerNode == move.Spec.SourceNode
	result.FSM.ConsumerExists = result.Consumer != nil
	result.FSM.EvictionRequested = move.Status.EvictionRequested
	result.FSM.PublishedOnSource = slices.Contains(state.PublishedNodes, move.Spec.SourceNode)
	result.FSM.CapacityApproved = move.Status.CapacityApproved
	result.FSM.CapacityBlocked = move.Status.CapacityReason != "" && !move.Status.CapacityApproved
	if result.FSM.CapacityBlocked {
		result.FSM.UnsafeReason = move.Status.CapacityReason
	}
	result.FSM.ReplacementExists = result.Replacement != nil
	result.FSM.ReplacementHeld = result.Replacement != nil && hasPlacementHold(result.Replacement)
}

// observeReplacement rejects a replacement Pod that the scheduler or a user has
// pinned somewhere this transaction never approved. Before eviction there is no
// replacement to judge, only the original consumer.
func observeReplacement(move volumeapi.Move, result *observation) {
	if result.Replacement == nil || preEviction(move) {
		return
	}
	if selected := result.Replacement.Spec.NodeSelector["kubernetes.io/hostname"]; selected != "" && !slices.Contains(result.CandidateNodes, selected) {
		result.FSM.DestinationBlocked = true
		result.FSM.UnsafeReason = "UnsupportedSchedulingConstraint"
	}
	if result.Replacement.Spec.NodeName != "" && result.DestinationNode != "" && result.Replacement.Spec.NodeName != result.DestinationNode {
		result.FSM.DestinationBlocked = true
		result.FSM.UnsafeReason = "InvalidDestination"
	}
}

func (r *Reconciler) observePlacement(ctx context.Context, move volumeapi.Move, result *observation) error {
	var placementErr error
	// A Move without a name or a phase owns no reservation: its create response
	// may have been lost before any status was persisted, and the placement Pod
	// name is derived from a Move name that no object carries yet.
	if move.Name != "" && move.Status.Phase != "" {
		placement, err := r.Client.CoreV1().Pods(r.Namespace).Get(ctx, result.Names.PlacementPod, metav1.GetOptions{})
		if err == nil {
			result.Placement = placement
		} else {
			placementErr = err
		}
	}
	if result.Placement == nil {
		if placementErr != nil && !apierrors.IsNotFound(placementErr) {
			return fmt.Errorf("read placement reservation Pod: %w", placementErr)
		}
		return nil
	}
	placement := result.Placement
	// Keep the workload held until the reservation is actually NotFound. A
	// deletion timestamp starts termination but does not prove that scheduler
	// capacity has been released or that the exact object has disappeared.
	result.FSM.PlacementExists = true
	if identityErr := validatePlacementIdentity(placement, move, result.Names); identityErr != nil {
		result.FSM.DestinationBlocked = true
		result.FSM.UnsafeReason = "PlacementReservationConflict"
	} else if placement.DeletionTimestamp != nil {
		// Wait for API disappearance before recreating the reservation or
		// releasing the held workload.
	} else if placement.Status.Phase == corev1.PodFailed {
		result.FSM.DestinationBlocked = true
		result.FSM.UnsafeReason = "PlacementReservationFailed"
	} else if placement.Spec.NodeName != "" {
		if move.Status.DestinationNode != "" && placement.Spec.NodeName != move.Status.DestinationNode {
			result.FSM.DestinationBlocked = true
			result.FSM.UnsafeReason = "InvalidDestination"
		} else if slices.Contains(result.CandidateNodes, placement.Spec.NodeName) {
			result.DestinationNode = placement.Spec.NodeName
			result.FSM.DestinationScheduled = true
		} else {
			result.FSM.DestinationBlocked = true
			result.FSM.UnsafeReason = "InvalidDestination"
		}
	}
	return nil
}

// observeDestination re-checks the selected destination against current Node and
// Pool readiness. repaired reports that readiness was accepted from a registered
// but not yet ready Pool inside this Move's own crash window.
func (r *Reconciler) observeDestination(ctx context.Context, move volumeapi.Move, pools poolIndex, result *observation) (bool, error) {
	if result.DestinationNode == "" {
		return false, nil
	}
	repaired := false
	readyPool, ready := pools.ready[result.DestinationNode]
	if !ready {
		registeredPool, exists := pools.registered[result.DestinationNode]
		staleAfter := r.PoolReadinessStaleAfter
		if staleAfter <= 0 {
			staleAfter = volumeapi.DefaultPoolReadinessStaleAfter
		}
		if exists && volumeapi.PoolReadyForActiveMoveRepairAt(registeredPool, move, result.Volume, r.now(), staleAfter) {
			readyPool, ready, repaired = registeredPool, true, true
		}
	}
	copyConflict := ready && volumeapi.PoolHasConflictingServingVolume(readyPool, move.Spec.VolumeID, move.Status.DestinationCopy)
	destinationNode, destinationErr := r.Client.CoreV1().Nodes().Get(ctx, result.DestinationNode, metav1.GetOptions{})
	if destinationErr != nil && !apierrors.IsNotFound(destinationErr) {
		return repaired, fmt.Errorf("read selected destination Node %q: %w", result.DestinationNode, destinationErr)
	}
	result.FSM.DestinationUnavailable = destinationErr != nil || !ready || !admission.NodeReady(destinationNode) || copyConflict
	if copyConflict {
		result.FSM.UnsafeReason = "DestinationServingCopyPresent"
	}
	if move.Status.CapacityApproved && ready &&
		(move.Status.DestinationPoolUID == "" || readyPool.UID != move.Status.DestinationPoolUID) {
		result.FSM.DestinationBlocked = true
		result.FSM.UnsafeReason = "DestinationPoolIdentityChanged"
	}
	return repaired, nil
}

func (r *Reconciler) observeJobs(ctx context.Context, move volumeapi.Move, repaired bool, result *observation) error {
	var err error
	result.FSM.CopyComplete, result.FSM.CopyFailed, err = r.jobState(ctx, result.Names.CopyJob)
	if err != nil {
		return err
	}
	result.FSM.PromotionComplete, result.FSM.PromotionFailed, err = r.jobState(ctx, result.Names.PromotionJob)
	if err != nil {
		return err
	}
	// A helper may repair its own exact unrecorded path, but a completed Job
	// must still wait for the next ordinary valid inventory before authority can
	// advance to promotion or owner commit.
	if repaired && (move.Status.Phase == string(fsm.PhaseCopying) && result.FSM.CopyComplete ||
		move.Status.Phase == string(fsm.PhasePromoting) && result.FSM.PromotionComplete) {
		result.FSM.DestinationUnavailable = true
	}
	return nil
}

func observeDestinationPublish(move volumeapi.Move, pools poolIndex, result *observation) {
	destinationPool, destinationReady := pools.ready[result.DestinationNode]
	result.FSM.PublishedOnDestination = result.DestinationNode != "" &&
		slices.Contains(result.Volume.PublishedNodes, result.DestinationNode) &&
		destinationReady &&
		volumeapi.PoolHasPublishedCopy(destinationPool, move.Status.DestinationCopy)
}

func (r *Reconciler) observeCleanup(ctx context.Context, move volumeapi.Move, result *observation) error {
	if move.Status.Phase != string(fsm.PhaseWaitingForDestinationPublish) && move.Status.Phase != string(fsm.PhaseCleaningSource) {
		return nil
	}
	complete, failed, err := r.cleanupState(ctx, move)
	if err != nil {
		return err
	}
	result.FSM.CleanupComplete, result.FSM.CleanupFailed = complete, failed
	return nil
}

func sourceCopyPresent(pool volumeapi.Pool, copy *volume.CopyIdentity) bool {
	if copy == nil || copy.Validate() != nil || copy.Role != volume.RoleServing ||
		copy.PoolName != pool.Name || copy.PoolUID != pool.UID || copy.NodeName != pool.NodeName || pool.Status.Inventory == nil {
		return false
	}
	for _, observed := range pool.Status.Inventory.Copies {
		if observed.Identity != nil && *observed.Identity == *copy {
			return observed.Present && observed.Problem == ""
		}
	}
	return false
}

func (r *Reconciler) jobState(ctx context.Context, name string) (complete, failed bool, err error) {
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read Job %q: %w", name, err)
	}
	for _, condition := range job.Status.Conditions {
		switch condition.Type {
		case batchv1.JobComplete:
			complete = condition.Status == corev1.ConditionTrue
		case batchv1.JobFailed:
			failed = condition.Status == corev1.ConditionTrue
		}
	}
	return complete, failed, nil
}

// validBinding is the single PV/PVC binding rule. Names can be reused after
// PVC/namespace deletion while Retain PVs and ShiftPVVolumes survive, so the
// pair must still be alive, still be this driver's volume, and still reference
// each other by exact UID. Each caller keeps its own diagnosis for a failure.
func validBinding(pv *corev1.PersistentVolume, claim *corev1.PersistentVolumeClaim, volumeID string) bool {
	if pv == nil || claim == nil {
		return false
	}
	ref := pv.Spec.ClaimRef
	return pv.DeletionTimestamp == nil && claim.DeletionTimestamp == nil &&
		pv.Spec.CSI != nil && pv.Spec.CSI.Driver == admission.DriverName && pv.Spec.CSI.VolumeHandle == volumeID &&
		ref != nil && ref.Namespace == claim.Namespace && ref.Name == claim.Name &&
		ref.UID != "" && ref.UID == claim.UID && claim.Spec.VolumeName == pv.Name
}

func podUsesClaim(pod *corev1.Pod, claimName string) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claimName {
			return true
		}
	}
	return false
}

func terminalPod(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func hasPlacementHold(pod *corev1.Pod) bool {
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == placementHoldName {
			return true
		}
	}
	return false
}
