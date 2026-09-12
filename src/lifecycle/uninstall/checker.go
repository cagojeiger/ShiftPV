package uninstall

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
	"github.com/cagojeiger/ShiftPV/src/pool/capacity"
)

const (
	DriverName                    = "csi.shiftpv.io"
	PoolProtectionFinalizer       = volumeapi.PoolProtectionFinalizer
	PoolIdentityReleaseAnnotation = volumeapi.PoolIdentityReleaseAnnotation
)

type VolumeRepository interface {
	ListVolumes(context.Context) (map[string]volumeapi.State, error)
	ListMoves(context.Context) ([]volumeapi.Move, error)
	ListPools(context.Context) ([]volumeapi.Pool, error)
	ListPoolRegistrations(context.Context) ([]volumeapi.Pool, error)
	RemovePoolFinalizer(context.Context, string, string) error
}

type CleanupRepository interface {
	List(context.Context) ([]cleanupapi.Cleanup, error)
}

type Checker struct {
	Client           kubernetes.Interface
	Volumes          VolumeRepository
	StorageClassName string
	Namespace        string
	Cleanups         CleanupRepository
	Now              func() time.Time
	InventoryMaxAge  time.Duration
}

type Blocker struct {
	Kind      string
	Namespace string
	Name      string
	Reason    string
}

type Report struct {
	Blockers []Blocker
}

const PoolInventoryBlockerKind = "ShiftPVPoolInventory"
const topologyKey = "topology.csi.shiftpv.io/node"

func (r Report) Safe() bool {
	return len(r.Blockers) == 0
}

func (c *Checker) Check(ctx context.Context) (Report, error) {
	return c.CheckAfter(ctx, time.Time{})
}

func (c *Checker) ReleasePoolProtection(ctx context.Context) error {
	if c == nil || c.Volumes == nil {
		return fmt.Errorf("uninstall checker is not configured")
	}
	pools, err := c.Volumes.ListPoolRegistrations(ctx)
	if err != nil {
		return fmt.Errorf("list ShiftPVPools: %w", err)
	}
	var result error
	for _, pool := range pools {
		if !slices.Contains(pool.Finalizers, PoolProtectionFinalizer) {
			continue
		}
		if err := c.Volumes.RemovePoolFinalizer(ctx, pool.Name, pool.UID); err != nil {
			result = errors.Join(result, fmt.Errorf("release ShiftPVPool %q protection: %w", pool.Name, err))
		}
	}
	return result
}

// CheckPoolDeleteAfter determines whether one exact Pool registration can be
// removed without losing authority over a volume, move, cleanup, reservation,
// PersistentVolume, or physical copy. Other Pools may remain in active use.
func (c *Checker) CheckPoolDeleteAfter(ctx context.Context, poolName string, poolUID types.UID, inventoryAfter time.Time) (Report, error) {
	if c == nil || c.Client == nil || c.Volumes == nil || c.Cleanups == nil {
		return Report{}, fmt.Errorf("Pool deletion checker is not configured")
	}
	if strings.TrimSpace(poolName) == "" || poolUID == "" {
		return Report{}, fmt.Errorf("exact Pool identity is required")
	}
	if strings.TrimSpace(c.Namespace) == "" {
		return Report{}, fmt.Errorf("ShiftPV namespace is required")
	}

	pools, err := c.Volumes.ListPoolRegistrations(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVPools: %w", err)
	}
	var target *volumeapi.Pool
	for index := range pools {
		if pools[index].Name == poolName {
			target = &pools[index]
			break
		}
	}
	if target == nil {
		return Report{}, fmt.Errorf("Pool %q was not found", poolName)
	}
	if target.UID != string(poolUID) {
		return Report{}, fmt.Errorf("Pool %q identity changed", poolName)
	}

	report := Report{}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	maxAge := c.InventoryMaxAge
	if maxAge <= 0 {
		maxAge = volumeapi.DefaultPoolReadinessStaleAfter
	}
	report.Blockers = append(report.Blockers, poolInventoryBlockers(*target, now, maxAge, inventoryAfter.UTC())...)

	persistentVolumes, err := c.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Report{}, fmt.Errorf("list PersistentVolumes: %w", err)
	}
	for _, persistentVolume := range persistentVolumes.Items {
		if persistentVolume.Spec.CSI == nil || persistentVolume.Spec.CSI.Driver != DriverName {
			continue
		}
		matches, known := persistentVolumeTargetsNode(persistentVolume, target.NodeName)
		if !known || matches {
			reason := "placement=unknown"
			if matches {
				reason = "node=" + target.NodeName
			}
			report.Blockers = append(report.Blockers, Blocker{Kind: "PersistentVolume", Name: persistentVolume.Name, Reason: reason})
		}
	}

	reservations, err := c.Client.CoreV1().ConfigMaps(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: capacity.ReservationSelector})
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPV volume reservations: %w", err)
	}
	for _, reservation := range reservations.Items {
		if reservation.Data["nodeName"] != target.NodeName {
			continue
		}
		report.Blockers = append(report.Blockers, Blocker{
			Kind: "VolumeReservation", Namespace: reservation.Namespace, Name: reservation.Name,
			Reason: "volume=" + reservation.Data["volumeID"] + " node=" + target.NodeName,
		})
	}

	volumes, err := c.Volumes.ListVolumes(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVVolumes: %w", err)
	}
	for volumeID, state := range volumes {
		usesPool := state.CurrentCopy != nil && state.CurrentCopy.PoolName == target.Name && state.CurrentCopy.PoolUID == target.UID
		usesPool = usesPool || contains(state.PublishedNodes, target.NodeName)
		if !usesPool && state.OwnerNode != target.NodeName {
			continue
		}
		reason := "owner=" + state.OwnerNode
		if state.CurrentCopy == nil {
			reason += " currentCopy=missing"
		} else {
			reason += " pool=" + state.CurrentCopy.PoolName + " poolUID=" + state.CurrentCopy.PoolUID
		}
		report.Blockers = append(report.Blockers, Blocker{Kind: "ShiftPVVolume", Name: volumeID, Reason: reason})
	}

	moves, err := c.Volumes.ListMoves(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVMoves: %w", err)
	}
	for _, move := range moves {
		phase := fsm.Phase(move.Status.Phase)
		if phase == fsm.PhaseSucceeded || phase == fsm.PhaseBlocked {
			continue
		}
		usesPool := move.Spec.SourceNode == target.NodeName || move.Status.DestinationNode == target.NodeName ||
			contains(move.Status.CandidateNodes, target.NodeName) || move.Status.DestinationPoolUID == target.UID ||
			copyUsesPool(move.Status.SourceCopy, *target) || copyUsesPool(move.Status.IncomingCopy, *target) || copyUsesPool(move.Status.DestinationCopy, *target)
		if usesPool {
			report.Blockers = append(report.Blockers, Blocker{Kind: "ShiftPVMove", Name: move.Name, Reason: fmt.Sprintf("phase=%s volume=%s", phase, move.Spec.VolumeID)})
		}
	}

	cleanups, err := c.Cleanups.List(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVCleanups: %w", err)
	}
	for _, cleanup := range cleanups {
		if cleanup.Status.Phase == cleanupapi.PhaseCompleted || cleanup.Spec.Target.PoolName != target.Name || cleanup.Spec.Target.PoolUID != target.UID {
			continue
		}
		report.Blockers = append(report.Blockers, Blocker{Kind: "ShiftPVCleanup", Name: cleanup.Name, Reason: "operation=" + cleanup.Spec.OperationID})
	}

	sort.Slice(report.Blockers, func(left, right int) bool {
		return blockerKey(report.Blockers[left]) < blockerKey(report.Blockers[right])
	})
	return report, nil
}

func copyUsesPool(copy *cleanupapi.CopyIdentity, pool volumeapi.Pool) bool {
	return copy != nil && copy.PoolName == pool.Name && copy.PoolUID == pool.UID
}

func persistentVolumeTargetsNode(persistentVolume corev1.PersistentVolume, nodeName string) (bool, bool) {
	affinity := persistentVolume.Spec.NodeAffinity
	if affinity == nil || affinity.Required == nil || len(affinity.Required.NodeSelectorTerms) == 0 {
		return false, false
	}
	for _, term := range affinity.Required.NodeSelectorTerms {
		known := false
		matches := false
		for _, expression := range term.MatchExpressions {
			if expression.Key != topologyKey {
				continue
			}
			switch expression.Operator {
			case corev1.NodeSelectorOpIn:
				known = true
				matches = contains(expression.Values, nodeName)
			case corev1.NodeSelectorOpNotIn:
				known = true
				matches = !contains(expression.Values, nodeName)
			}
		}
		if !known {
			return false, false
		}
		if matches {
			return true, true
		}
	}
	return false, true
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (c *Checker) CheckAfter(ctx context.Context, inventoryAfter time.Time) (Report, error) {
	if c == nil || c.Client == nil || c.Volumes == nil || c.Cleanups == nil {
		return Report{}, fmt.Errorf("uninstall checker is not configured")
	}
	if strings.TrimSpace(c.StorageClassName) == "" {
		return Report{}, fmt.Errorf("ShiftPV StorageClass name is required")
	}
	if strings.TrimSpace(c.Namespace) == "" {
		return Report{}, fmt.Errorf("ShiftPV namespace is required")
	}

	report := Report{}
	persistentVolumes, err := c.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Report{}, fmt.Errorf("list PersistentVolumes: %w", err)
	}
	for _, persistentVolume := range persistentVolumes.Items {
		if persistentVolume.Spec.CSI == nil || persistentVolume.Spec.CSI.Driver != DriverName {
			continue
		}
		reason := fmt.Sprintf("driver=%s volumeHandle=%s", DriverName, persistentVolume.Spec.CSI.VolumeHandle)
		if persistentVolume.Spec.ClaimRef != nil {
			reason += fmt.Sprintf(" claim=%s/%s", persistentVolume.Spec.ClaimRef.Namespace, persistentVolume.Spec.ClaimRef.Name)
		}
		report.Blockers = append(report.Blockers, Blocker{Kind: "PersistentVolume", Name: persistentVolume.Name, Reason: reason})
	}

	claims, err := c.Client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return Report{}, fmt.Errorf("list PersistentVolumeClaims: %w", err)
	}
	for _, claim := range claims.Items {
		if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != c.StorageClassName {
			continue
		}
		reason := "references the ShiftPV StorageClass"
		if claim.Spec.VolumeName != "" {
			reason += " volume=" + claim.Spec.VolumeName
		}
		report.Blockers = append(report.Blockers, Blocker{Kind: "PersistentVolumeClaim", Namespace: claim.Namespace, Name: claim.Name, Reason: reason})
	}

	reservations, err := c.Client.CoreV1().ConfigMaps(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: capacity.ReservationSelector})
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPV volume reservations: %w", err)
	}
	for _, reservation := range reservations.Items {
		reasonParts := []string{"volume=" + reservation.Data["volumeID"]}
		if reservation.Data["volumeUID"] != "" {
			reasonParts = append(reasonParts, "volumeUID="+reservation.Data["volumeUID"])
		}
		if reservation.Data["nodeName"] != "" {
			reasonParts = append(reasonParts, "node="+reservation.Data["nodeName"])
		}
		report.Blockers = append(report.Blockers, Blocker{
			Kind:      "VolumeReservation",
			Namespace: reservation.Namespace,
			Name:      reservation.Name,
			Reason:    strings.Join(reasonParts, " "),
		})
	}

	volumes, err := c.Volumes.ListVolumes(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVVolumes: %w", err)
	}
	for volumeID, state := range volumes {
		reasonParts := []string{"phase=" + state.Phase, "owner=" + state.OwnerNode}
		if state.ActiveMove != "" {
			reasonParts = append(reasonParts, "activeMove="+state.ActiveMove)
		}
		if len(state.PublishedNodes) > 0 {
			reasonParts = append(reasonParts, "publishedNodes="+strings.Join(state.PublishedNodes, ","))
		}
		report.Blockers = append(report.Blockers, Blocker{Kind: "ShiftPVVolume", Name: volumeID, Reason: strings.Join(reasonParts, " ")})
	}

	moves, err := c.Volumes.ListMoves(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVMoves: %w", err)
	}
	for _, move := range moves {
		phase := fsm.Phase(move.Status.Phase)
		if phase == fsm.PhaseSucceeded || phase == fsm.PhaseBlocked {
			continue
		}
		report.Blockers = append(report.Blockers, Blocker{
			Kind:   "ShiftPVMove",
			Name:   move.Name,
			Reason: fmt.Sprintf("phase=%s volume=%s", phase, move.Spec.VolumeID),
		})
	}
	requests, err := c.Cleanups.List(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVCleanups: %w", err)
	}
	for _, request := range requests {
		if request.Status.Phase == cleanupapi.PhaseCompleted {
			continue
		}
		reason := fmt.Sprintf("phase=%s operation=%s volume=%s", request.Status.Phase, request.Spec.OperationID, request.Spec.Target.VolumeID)
		report.Blockers = append(report.Blockers, Blocker{Kind: "ShiftPVCleanup", Name: request.Name, Reason: reason})
	}

	pools, err := c.Volumes.ListPools(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVPools: %w", err)
	}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	maxAge := c.InventoryMaxAge
	if maxAge <= 0 {
		maxAge = volumeapi.DefaultPoolReadinessStaleAfter
	}
	for _, pool := range pools {
		report.Blockers = append(report.Blockers, poolInventoryBlockers(pool, now, maxAge, inventoryAfter.UTC())...)
		if pool.DeletionTimestamp != nil {
			released := meta.FindStatusCondition(pool.Status.Conditions, volumeapi.PoolConditionIdentityReleased)
			if released == nil || released.Status != metav1.ConditionTrue || released.ObservedGeneration != pool.Generation {
				reason := "identity=retained"
				if released != nil && released.Reason != "" {
					reason = released.Reason
				}
				report.Blockers = append(report.Blockers, Blocker{Kind: "ShiftPVPoolIdentity", Name: pool.Name, Reason: reason})
			}
		}
	}

	sort.Slice(report.Blockers, func(left, right int) bool {
		leftKey := blockerKey(report.Blockers[left])
		rightKey := blockerKey(report.Blockers[right])
		return leftKey < rightKey
	})
	return report, nil
}

func (r Report) WaitingForInventory() bool {
	if len(r.Blockers) == 0 {
		return false
	}
	for _, blocker := range r.Blockers {
		if blocker.Kind != PoolInventoryBlockerKind && blocker.Kind != "ShiftPVPoolIdentity" {
			return false
		}
	}
	return true
}

func poolInventoryBlockers(pool volumeapi.Pool, now time.Time, maxAge time.Duration, observedAfter time.Time) []Blocker {
	inventory := pool.Status.Inventory
	if inventory == nil {
		return []Blocker{{Kind: PoolInventoryBlockerKind, Name: pool.Name, Reason: "inventory=missing"}}
	}
	result := []Blocker{}
	reasons := []string{}
	if pool.Status.ObservedGeneration != pool.Generation {
		reasons = append(reasons, fmt.Sprintf("generation=%d observedGeneration=%d", pool.Generation, pool.Status.ObservedGeneration))
	}
	if !inventory.Valid {
		reasons = append(reasons, "valid=false")
	}
	if inventory.Truncated {
		reasons = append(reasons, "truncated=true")
	}
	if inventory.Message != "" {
		reasons = append(reasons, "message="+inventory.Message)
	}
	observedAt := inventory.ObservedAt.Time
	switch {
	case inventory.ObservedAt.IsZero():
		reasons = append(reasons, "observedAt=missing")
	case now.Before(observedAt):
		reasons = append(reasons, "observedAt=future")
	case now.Sub(observedAt) > maxAge:
		reasons = append(reasons, "observedAt=stale")
	case !observedAfter.IsZero() && !observedAt.After(observedAfter):
		reasons = append(reasons, "observedAt=before-quiesce")
	}
	if len(reasons) > 0 {
		result = append(result, Blocker{Kind: PoolInventoryBlockerKind, Name: pool.Name, Reason: strings.Join(reasons, " ")})
		// Copy observations are authoritative only when the whole inventory is
		// generation-current, valid, complete, fresh, and newer than the
		// uninstall quiesce barrier. Until then, wait for a trustworthy snapshot
		// instead of turning stale copy evidence into a terminal blocker.
		return result
	}
	for _, observed := range inventory.Copies {
		if !observed.Present && !observed.Published {
			continue
		}
		parts := []string{"marker=" + observed.Marker, fmt.Sprintf("present=%t", observed.Present)}
		if observed.Identity != nil {
			parts = append(parts, "role="+string(observed.Identity.Role), "volume="+observed.Identity.VolumeID, "copy="+observed.Identity.CopyID)
		}
		if observed.Published {
			parts = append(parts, "published=true")
		}
		if observed.Problem != "" {
			parts = append(parts, "problem="+observed.Problem)
		}
		result = append(result, Blocker{Kind: "ShiftPVPoolCopy", Name: pool.Name, Reason: strings.Join(parts, " ")})
	}
	return result
}

func blockerKey(blocker Blocker) string {
	return blocker.Kind + "\x00" + blocker.Namespace + "\x00" + blocker.Name + "\x00" + blocker.Reason
}
