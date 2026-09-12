package volumeapi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

var (
	VolumeResource = schema.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvvolumes"}
	PoolResource   = schema.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvpools"}
	MoveResource   = schema.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvmoves"}

	ErrStateConflict     = errors.New("ShiftPV state precondition failed")
	ErrPoolConfiguration = errors.New("ShiftPV Pool configuration is invalid")
	ErrPoolNotFound      = errors.New("ShiftPV Pool is not registered")
	ErrPoolNotReady      = errors.New("ShiftPV Pool is not ready")
	ErrPoolCopyConflict  = errors.New("ShiftPV Pool contains a conflicting serving copy")
)

const (
	PoolConditionReady            = "Ready"
	PoolConditionAccessible       = "Accessible"
	PoolConditionIdentityReleased = "IdentityReleased"
	PoolProtectionFinalizer       = "shiftpv.io/pool-protection"
	PoolIdentityReleaseAnnotation = "shiftpv.io/release-pool-identity"
	// PoolConditionMounted is retained so newer node plugins can remove the
	// obsolete condition written by releases that required an exact mount point.
	PoolConditionMounted           = "Mounted"
	PoolConditionWritable          = "Writable"
	PoolConditionCapacityReadable  = "CapacityReadable"
	DefaultPoolReadinessStaleAfter = 3 * time.Minute
)

const (
	PhasePending  = "Pending"
	PhaseReady    = "Ready"
	PhaseDeleting = "Deleting"
	PhaseMoving   = "Moving"
	PhaseBlocked  = "Blocked"
)

type State struct {
	UID                 string
	Phase               string
	OwnerNode           string
	ActiveMove          string
	PublishedNodes      []string
	CreationOperationID string
	DeletionOperationID string
	CurrentCopy         *volume.CopyIdentity
}

type CopyAuthority int

const (
	CopyAuthorityNone CopyAuthority = iota
	CopyAuthorityCurrent
	CopyAuthoritySuperseded
	CopyAuthorityUncertain
)

// ClassifyCopyAuthority determines whether the exact physical copy is still
// owned by a live volume incarnation. A different, internally consistent
// current copy proves that the target has been superseded; incomplete or
// contradictory state remains fail-closed.
func ClassifyCopyAuthority(states map[string]State, target volume.CopyIdentity) CopyAuthority {
	matched := 0
	for volumeID, state := range states {
		if state.CurrentCopy != nil && *state.CurrentCopy == target {
			return CopyAuthorityCurrent
		}
		if volumeID != target.VolumeID && state.UID != target.VolumeUID {
			continue
		}
		matched++
		if state.CurrentCopy == nil || state.CurrentCopy.Validate() != nil ||
			state.CurrentCopy.VolumeID != volumeID || state.CurrentCopy.VolumeUID != state.UID {
			return CopyAuthorityUncertain
		}
	}
	if matched == 0 {
		return CopyAuthorityNone
	}
	if matched == 1 {
		return CopyAuthoritySuperseded
	}
	return CopyAuthorityUncertain
}

func CreationOperationID(volumeUID string) (string, error) {
	operationID := "create-" + volumeUID
	if !volume.ValidIdentityToken(volumeUID) || !volume.ValidIdentityToken(operationID) {
		return "", fmt.Errorf("invalid volume creation operation identity")
	}
	return operationID, nil
}

type Pool struct {
	Name                    string
	UID                     string
	NodeName                string
	MountPath               string
	CapacityLimit           string
	Generation              int64
	DeletionTimestamp       *metav1.Time
	Finalizers              []string
	IdentityReleaseApproval string
	Status                  PoolStatus
}

type PoolStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastProbeTime      metav1.Time        `json:"lastProbeTime,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	Inventory          *PoolInventory     `json:"inventory,omitempty"`
}

type PoolInventory struct {
	ObservedAt metav1.Time       `json:"observedAt"`
	Valid      bool              `json:"valid"`
	Truncated  bool              `json:"truncated,omitempty"`
	Message    string            `json:"message,omitempty"`
	Copies     []CopyObservation `json:"copies,omitempty"`
}

type CopyObservation struct {
	Marker    string               `json:"marker"`
	Identity  *volume.CopyIdentity `json:"identity,omitempty"`
	Present   bool                 `json:"present"`
	Published bool                 `json:"published,omitempty"`
	Problem   string               `json:"problem,omitempty"`
}

type MoveSpec struct {
	VolumeID   string
	SourceNode string
	Recovery   string
}

type MoveStatus struct {
	Phase                string
	Reason               string
	Message              string
	LastTransitionTime   string
	LastProgressTime     string
	PersistentVolumeName string
	ClaimNamespace       string
	ClaimName            string
	ConsumerName         string
	ConsumerUID          string
	ReplacementName      string
	ReplacementUID       string
	DestinationNode      string
	DestinationPoolUID   string
	SourceBytes          int64
	CapacityApproved     bool
	CapacityReason       string
	CandidateNodes       []string
	EvictionRequested    bool
	CopyJobName          string
	PromotionJobName     string
	CleanupJobName       string
	CopyOperationID      string
	PromotionOperationID string
	CleanupName          string
	SourceCopy           *volume.CopyIdentity
	IncomingCopy         *volume.CopyIdentity
	DestinationCopy      *volume.CopyIdentity
	RecoveryPhase        string
	RecoveryOwner        string
	RecoveryReason       string
	RecoveryMessage      string
}

type Move struct {
	Name            string
	UID             string
	ResourceVersion string
	Spec            MoveSpec
	Status          MoveStatus
}

func MoveReservesDestination(move Move, state State, nodeName string) bool {
	return move.Status.CapacityApproved &&
		move.Status.DestinationNode == nodeName &&
		state.OwnerNode != nodeName &&
		state.ActiveMove == move.Name
}

type Registry struct {
	Client                  dynamic.Interface
	PoolReadinessStaleAfter time.Duration
	Now                     func() time.Time
}

func (r *Registry) Ensure(ctx context.Context, volumeID, ownerNode string) error {
	if err := r.validate(); err != nil {
		return err
	}
	resource := r.Client.Resource(VolumeResource)
	object, err := resource.Get(ctx, volumeID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		object, err = resource.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "shiftpv.io/v1alpha1",
			"kind":       "ShiftPVVolume",
			"metadata":   map[string]any{"name": volumeID},
			"spec":       map[string]any{"volumeID": volumeID},
		}}, metav1.CreateOptions{})
	}
	if err != nil {
		return fmt.Errorf("ensure ShiftPVVolume: %w", err)
	}
	state, err := stateFrom(object)
	if err != nil {
		return err
	}
	if state.Phase != "" {
		if state.OwnerNode != ownerNode {
			return fmt.Errorf("volume %q is owned by node %q, not %q", volumeID, state.OwnerNode, ownerNode)
		}
		return nil
	}
	return r.SetState(ctx, volumeID, State{Phase: PhaseReady, OwnerNode: ownerNode})
}

// BeginCreate persists the exact copy identity before node-local filesystem work.
func (r *Registry) BeginCreate(ctx context.Context, volumeID, ownerNode string) (State, error) {
	if err := r.validate(); err != nil {
		return State{}, err
	}
	resource := r.Client.Resource(VolumeResource)
	object, err := resource.Get(ctx, volumeID, metav1.GetOptions{})
	create := apierrors.IsNotFound(err)
	if err == nil {
		state, stateErr := stateFrom(object)
		if stateErr != nil {
			return State{}, stateErr
		}
		if state.Phase != "" {
			return r.resumeCreate(ctx, object, state, volumeID, ownerNode)
		}
	} else if !apierrors.IsNotFound(err) {
		return State{}, fmt.Errorf("read ShiftPVVolume creation intent: %w", err)
	}

	installationID, err := r.InstallationID(ctx)
	if err != nil {
		return State{}, err
	}
	pool, err := r.ReadyPoolForNode(ctx, ownerNode)
	if err != nil {
		return State{}, err
	}
	if PoolHasServingVolume(pool, volumeID) {
		return State{}, fmt.Errorf("%w: Pool %q already contains volume %q", ErrPoolCopyConflict, pool.Name, volumeID)
	}
	if create {
		object, err = resource.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "shiftpv.io/v1alpha1",
			"kind":       "ShiftPVVolume",
			"metadata":   map[string]any{"name": volumeID},
			"spec":       map[string]any{"volumeID": volumeID},
		}}, metav1.CreateOptions{})
	}
	if err != nil {
		return State{}, fmt.Errorf("begin ShiftPVVolume creation: %w", err)
	}
	if object.GetUID() == "" || pool.UID == "" {
		return State{}, fmt.Errorf("%w: Kubernetes object identity is missing", ErrStateConflict)
	}
	operationID, err := CreationOperationID(string(object.GetUID()))
	if err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrStateConflict, err)
	}
	copy := volume.CopyIdentity{
		InstallationID: installationID,
		PoolName:       pool.Name,
		PoolUID:        pool.UID,
		VolumeID:       volumeID,
		VolumeUID:      string(object.GetUID()),
		CopyID:         "initial-" + string(object.GetUID()),
		NodeName:       ownerNode,
		Role:           volume.RoleServing,
	}
	if err := copy.Validate(); err != nil {
		return State{}, fmt.Errorf("build creation identity: %w", err)
	}
	state, err := stateFrom(object)
	if err != nil {
		return State{}, err
	}
	if state.Phase == "" {
		next := State{
			UID:                 string(object.GetUID()),
			Phase:               PhasePending,
			OwnerNode:           ownerNode,
			CreationOperationID: operationID,
			CurrentCopy:         &copy,
		}
		if err := r.mutateState(ctx, volumeID, func(current State) (State, error) {
			if current.UID != next.UID {
				return State{}, fmt.Errorf("%w: ShiftPVVolume %q UID changed from %q to %q", ErrStateConflict, volumeID, next.UID, current.UID)
			}
			if current.Phase != "" {
				return current, nil
			}
			return next, nil
		}); err != nil {
			return State{}, err
		}
		state, err = r.Get(ctx, volumeID)
		if err != nil {
			return State{}, err
		}
	}
	return r.resumeCreate(ctx, object, state, volumeID, ownerNode)
}

func (r *Registry) resumeCreate(ctx context.Context, object *unstructured.Unstructured, state State, volumeID, ownerNode string) (State, error) {
	if object == nil || object.GetUID() == "" || state.UID != string(object.GetUID()) || state.OwnerNode != ownerNode ||
		state.CurrentCopy == nil || state.CurrentCopy.Validate() != nil || state.CurrentCopy.VolumeID != volumeID ||
		state.CurrentCopy.VolumeUID != state.UID || state.CurrentCopy.NodeName != ownerNode || state.CurrentCopy.Role != volume.RoleServing ||
		(state.Phase != PhasePending && state.Phase != PhaseReady) {
		return State{}, fmt.Errorf("%w: volume creation identity changed", ErrStateConflict)
	}
	operationID, err := CreationOperationID(state.UID)
	if err != nil || state.CreationOperationID != operationID {
		return State{}, fmt.Errorf("%w: volume creation operation changed", ErrStateConflict)
	}
	installationID, err := r.InstallationID(ctx)
	if err != nil {
		return State{}, err
	}
	pool, err := r.PoolForNode(ctx, ownerNode)
	if err != nil {
		return State{}, err
	}
	if state.CurrentCopy.InstallationID != installationID || state.CurrentCopy.PoolName != pool.Name || state.CurrentCopy.PoolUID != pool.UID {
		return State{}, fmt.Errorf("%w: volume creation Pool identity changed", ErrStateConflict)
	}
	return state, nil
}

func (r *Registry) CompleteCreate(ctx context.Context, volumeID, uid string, copy volume.CopyIdentity) error {
	operationID, err := CreationOperationID(uid)
	if err != nil {
		return ErrStateConflict
	}
	return r.mutateState(ctx, volumeID, func(current State) (State, error) {
		if uid == "" || current.UID != uid || current.CurrentCopy == nil || *current.CurrentCopy != copy ||
			current.CreationOperationID != operationID || current.OwnerNode != copy.NodeName {
			return State{}, ErrStateConflict
		}
		if current.Phase == PhaseReady {
			return current, nil
		}
		if current.Phase != PhasePending {
			return State{}, ErrStateConflict
		}
		current.Phase = PhaseReady
		return current, nil
	})
}

// BeginDelete fences new publications before an approved filesystem cleanup
// can be created. The deletion operation is durable and idempotent across
// controller restarts.
func (r *Registry) BeginDelete(ctx context.Context, volumeID, uid string, copy volume.CopyIdentity) (State, error) {
	operationID := "delete-" + uid
	if uid == "" || copy.Validate() != nil || copy.VolumeID != volumeID || copy.VolumeUID != uid {
		return State{}, ErrStateConflict
	}
	err := r.mutateState(ctx, volumeID, func(current State) (State, error) {
		if current.UID != uid || current.OwnerNode != copy.NodeName || current.CurrentCopy == nil || *current.CurrentCopy != copy ||
			current.ActiveMove != "" || len(current.PublishedNodes) != 0 {
			return State{}, ErrStateConflict
		}
		switch current.Phase {
		case PhaseReady:
			if current.DeletionOperationID != "" {
				return State{}, ErrStateConflict
			}
			current.Phase = PhaseDeleting
			current.DeletionOperationID = operationID
		case PhaseDeleting:
			if current.DeletionOperationID != operationID {
				return State{}, ErrStateConflict
			}
		default:
			return State{}, ErrStateConflict
		}
		return current, nil
	})
	if err != nil {
		return State{}, err
	}
	state, err := r.Get(ctx, volumeID)
	if err != nil {
		return State{}, err
	}
	if state.Phase != PhaseDeleting || state.DeletionOperationID != operationID || state.UID != uid || state.CurrentCopy == nil || *state.CurrentCopy != copy {
		return State{}, ErrStateConflict
	}
	return state, nil
}

func (r *Registry) Get(ctx context.Context, volumeID string) (State, error) {
	object, err := r.getVolume(ctx, volumeID)
	if err != nil {
		return State{}, err
	}
	return stateFrom(object)
}

func (r *Registry) ListVolumes(ctx context.Context) (map[string]State, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	list, err := r.Client.Resource(VolumeResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ShiftPVVolume: %w", err)
	}
	result := make(map[string]State, len(list.Items))
	for index := range list.Items {
		state, stateErr := stateFrom(&list.Items[index])
		if stateErr != nil {
			return nil, fmt.Errorf("decode ShiftPVVolume %q: %w", list.Items[index].GetName(), stateErr)
		}
		result[list.Items[index].GetName()] = state
	}
	return result, nil
}

func (r *Registry) Delete(ctx context.Context, volumeID, uid string) error {
	if err := r.validate(); err != nil {
		return err
	}
	if uid == "" {
		return fmt.Errorf("ShiftPVVolume UID is required for deletion")
	}
	precondition := types.UID(uid)
	err := r.Client.Resource(VolumeResource).Delete(ctx, volumeID, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &precondition}})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ShiftPVVolume: %w", err)
	}
	return nil
}

func (r *Registry) SetState(ctx context.Context, volumeID string, state State) error {
	return r.mutateState(ctx, volumeID, func(State) (State, error) { return state, nil })
}

func (r *Registry) CompareAndSetState(ctx context.Context, volumeID, expectedPhase, expectedActiveMove, expectedOwner string, next State) error {
	return r.mutateState(ctx, volumeID, func(current State) (State, error) {
		if next.UID == "" || current.UID != next.UID || current.Phase != expectedPhase || current.ActiveMove != expectedActiveMove || current.OwnerNode != expectedOwner {
			return State{}, fmt.Errorf("%w: volume %q is phase=%q activeMove=%q owner=%q", ErrStateConflict, volumeID, current.Phase, current.ActiveMove, current.OwnerNode)
		}
		// Node publication is independently maintained by the CSI node service.
		// A controller's earlier observation must not erase a concurrent publish/unpublish.
		next.PublishedNodes = current.PublishedNodes
		if next.Phase == PhaseReady {
			for _, node := range current.PublishedNodes {
				if node != next.OwnerNode {
					return State{}, fmt.Errorf("%w: node %q is still published", ErrStateConflict, node)
				}
			}
		}
		return next, nil
	})
}

func (r *Registry) SetPublished(ctx context.Context, volumeID, nodeName string, published bool) error {
	return r.mutateState(ctx, volumeID, func(state State) (State, error) {
		nodes := make(map[string]struct{}, len(state.PublishedNodes)+1)
		for _, node := range state.PublishedNodes {
			nodes[node] = struct{}{}
		}
		if published {
			nodes[nodeName] = struct{}{}
		} else {
			delete(nodes, nodeName)
		}
		state.PublishedNodes = state.PublishedNodes[:0]
		for node := range nodes {
			state.PublishedNodes = append(state.PublishedNodes, node)
		}
		sort.Strings(state.PublishedNodes)
		return state, nil
	})
}

func (r *Registry) BeginPublish(ctx context.Context, volumeID, nodeName string, copy volume.CopyIdentity) error {
	return r.mutateState(ctx, volumeID, func(state State) (State, error) {
		if state.Phase != PhaseReady || state.OwnerNode != nodeName || state.CurrentCopy == nil || *state.CurrentCopy != copy {
			return State{}, fmt.Errorf("%w: volume is not publishable by this copy", ErrStateConflict)
		}
		nodes := make(map[string]struct{}, len(state.PublishedNodes)+1)
		for _, node := range state.PublishedNodes {
			nodes[node] = struct{}{}
		}
		nodes[nodeName] = struct{}{}
		state.PublishedNodes = state.PublishedNodes[:0]
		for node := range nodes {
			state.PublishedNodes = append(state.PublishedNodes, node)
		}
		sort.Strings(state.PublishedNodes)
		return state, nil
	})
}

// ReconcilePublished changes publication state only for the exact live copy.
// It is used after inspecting real mount references under the node-local lock.
func (r *Registry) ReconcilePublished(ctx context.Context, volumeID, nodeName string, copy volume.CopyIdentity, published bool) error {
	return r.mutateState(ctx, volumeID, func(state State) (State, error) {
		if copy.Validate() != nil || copy.Role != volume.RoleServing || copy.VolumeID != volumeID || copy.NodeName != nodeName ||
			state.UID != copy.VolumeUID || state.OwnerNode != nodeName || state.CurrentCopy == nil || *state.CurrentCopy != copy {
			return State{}, fmt.Errorf("%w: publication copy identity changed", ErrStateConflict)
		}
		if published && state.Phase != PhaseReady {
			return State{}, fmt.Errorf("%w: volume is not publishable", ErrStateConflict)
		}
		nodes := make(map[string]struct{}, len(state.PublishedNodes)+1)
		for _, node := range state.PublishedNodes {
			nodes[node] = struct{}{}
		}
		if published {
			nodes[nodeName] = struct{}{}
		} else {
			delete(nodes, nodeName)
		}
		state.PublishedNodes = state.PublishedNodes[:0]
		for node := range nodes {
			state.PublishedNodes = append(state.PublishedNodes, node)
		}
		sort.Strings(state.PublishedNodes)
		return state, nil
	})
}

func (r *Registry) Pools(ctx context.Context) ([]Pool, error) {
	pools, err := r.ListPools(ctx)
	if err == nil && len(pools) == 0 {
		return nil, fmt.Errorf("%w: no ShiftPVPool nodes are registered", ErrPoolConfiguration)
	}
	return pools, err
}

// ListPools permits an empty registry for read-only inventory.
func (r *Registry) ListPools(ctx context.Context) ([]Pool, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	list, err := r.Client.Resource(PoolResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ShiftPVPool: %w", err)
	}
	result := make([]Pool, 0, len(list.Items))
	nodes := make(map[string]struct{}, len(list.Items))
	for index := range list.Items {
		pool, poolErr := poolFrom(&list.Items[index])
		if poolErr != nil {
			return nil, poolErr
		}
		if pool.NodeName == "" || !filepath.IsAbs(pool.MountPath) || pool.MountPath == "/" {
			return nil, fmt.Errorf("%w: ShiftPVPool %q has invalid nodeName or mountPath", ErrPoolConfiguration, list.Items[index].GetName())
		}
		if _, duplicate := nodes[pool.NodeName]; duplicate {
			return nil, fmt.Errorf("%w: multiple ShiftPVPools are registered for node %q", ErrPoolConfiguration, pool.NodeName)
		}
		nodes[pool.NodeName] = struct{}{}
		result = append(result, pool)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].NodeName < result[right].NodeName })
	return result, nil
}

func (r *Registry) ReadyPools(ctx context.Context) ([]Pool, error) {
	pools, err := r.Pools(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	staleAfter := r.PoolReadinessStaleAfter
	if staleAfter <= 0 {
		staleAfter = DefaultPoolReadinessStaleAfter
	}
	ready := make([]Pool, 0, len(pools))
	for _, pool := range pools {
		poolReady, _ := pool.ReadyAt(now, staleAfter)
		inventoryReady, _ := poolInventoryReadyAt(pool, now, staleAfter)
		if poolReady && inventoryReady {
			ready = append(ready, pool)
		}
	}
	return ready, nil
}

func (r *Registry) PoolNodes(ctx context.Context) ([]string, error) {
	// Accessible topology is the durable mobility universe encoded into the PV.
	// Keep every registered Pool here even if one is temporarily not Ready;
	// current readiness is enforced when selecting a provisioning or move target.
	pools, err := r.Pools(ctx)
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]struct{}, len(pools))
	for _, pool := range pools {
		nodes[pool.NodeName] = struct{}{}
	}
	result := make([]string, 0, len(nodes))
	for node := range nodes {
		result = append(result, node)
	}
	sort.Strings(result)
	return result, nil
}

func (r *Registry) PoolForNode(ctx context.Context, nodeName string) (Pool, error) {
	if nodeName == "" {
		return Pool{}, fmt.Errorf("%w: node name is required", ErrPoolConfiguration)
	}
	pools, err := r.Pools(ctx)
	if err != nil {
		return Pool{}, err
	}
	var result Pool
	for _, pool := range pools {
		if pool.NodeName != nodeName {
			continue
		}
		if result.NodeName != "" {
			return Pool{}, fmt.Errorf("%w: multiple ShiftPVPools are registered for node %q", ErrPoolConfiguration, nodeName)
		}
		result = pool
	}
	if result.NodeName == "" {
		return Pool{}, fmt.Errorf("%w: no ShiftPVPool is registered for node %q", ErrPoolNotFound, nodeName)
	}
	return result, nil
}

func (r *Registry) ReadyPoolForNode(ctx context.Context, nodeName string) (Pool, error) {
	pool, err := r.PoolForNode(ctx, nodeName)
	if err != nil {
		return Pool{}, err
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	staleAfter := r.PoolReadinessStaleAfter
	if staleAfter <= 0 {
		staleAfter = DefaultPoolReadinessStaleAfter
	}
	if ready, reason := pool.ReadyAt(now, staleAfter); !ready {
		return Pool{}, fmt.Errorf("%w: ShiftPVPool %q on node %q: %s", ErrPoolNotReady, pool.Name, nodeName, reason)
	}
	if ready, reason := poolInventoryReadyAt(pool, now, staleAfter); !ready {
		return Pool{}, fmt.Errorf("%w: ShiftPVPool %q on node %q: %s", ErrPoolNotReady, pool.Name, nodeName, reason)
	}
	return pool, nil
}

func poolInventoryReadyAt(pool Pool, now time.Time, staleAfter time.Duration) (bool, string) {
	inventory := pool.Status.Inventory
	if inventory == nil {
		return false, "InventoryMissing"
	}
	if !inventory.Valid {
		return false, "InventoryInvalid"
	}
	if inventory.Truncated {
		return false, "InventoryTruncated"
	}
	if inventory.ObservedAt.IsZero() || now.Before(inventory.ObservedAt.Time) || now.Sub(inventory.ObservedAt.Time) > staleAfter {
		return false, "InventoryStale"
	}
	return true, ""
}

// PoolHasServingVolume reports whether the Pool already contains the physical
// serving path reserved for volumeID, regardless of that copy's incarnation.
func PoolHasServingVolume(pool Pool, volumeID string) bool {
	return PoolHasConflictingServingVolume(pool, volumeID, nil)
}

// PoolHasConflictingServingVolume permits only the exact serving copy already
// journaled by the current transaction.
func PoolHasConflictingServingVolume(pool Pool, volumeID string, allowed *volume.CopyIdentity) bool {
	if pool.Status.Inventory == nil {
		return false
	}
	for _, observed := range pool.Status.Inventory.Copies {
		if observed.Present && observed.Identity != nil && observed.Identity.Role == volume.RoleServing && observed.Identity.VolumeID == volumeID {
			if allowed != nil && *observed.Identity == *allowed {
				continue
			}
			return true
		}
	}
	return false
}

func (r *Registry) SetPoolStatus(ctx context.Context, name, uid, nodeName string, status PoolStatus) error {
	if err := r.validate(); err != nil {
		return err
	}
	if name == "" || uid == "" || nodeName == "" {
		return fmt.Errorf("ShiftPVPool name, UID, and node name are required")
	}
	resource := r.Client.Resource(PoolResource)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		object, err := resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read ShiftPVPool status: %w", err)
		}
		if string(object.GetUID()) != uid {
			return fmt.Errorf("%w: ShiftPVPool %q UID changed from %q to %q", ErrStateConflict, name, uid, object.GetUID())
		}
		registeredNode, _, _ := unstructured.NestedString(object.Object, "spec", "nodeName")
		if registeredNode != nodeName {
			return fmt.Errorf("%w: ShiftPVPool %q belongs to node %q, not %q", ErrStateConflict, name, registeredNode, nodeName)
		}
		data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
		if err != nil {
			return fmt.Errorf("encode ShiftPVPool status: %w", err)
		}
		if err := unstructured.SetNestedMap(object.Object, data, "status"); err != nil {
			return fmt.Errorf("set ShiftPVPool status: %w", err)
		}
		if _, err := resource.UpdateStatus(ctx, object, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update ShiftPVPool status: %w", err)
		}
		return nil
	})
}

func (r *Registry) EnsurePoolFinalizer(ctx context.Context, name, uid string) error {
	return r.updatePoolFinalizer(ctx, name, uid, true)
}

func (r *Registry) RemovePoolFinalizer(ctx context.Context, name, uid string) error {
	return r.updatePoolFinalizer(ctx, name, uid, false)
}

func (r *Registry) ApprovePoolIdentityRelease(ctx context.Context, name, uid string) error {
	if err := r.validate(); err != nil {
		return err
	}
	if name == "" || uid == "" {
		return fmt.Errorf("ShiftPVPool name and UID are required")
	}
	resource := r.Client.Resource(PoolResource)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		object, err := resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read ShiftPVPool identity release approval: %w", err)
		}
		if string(object.GetUID()) != uid {
			return fmt.Errorf("%w: ShiftPVPool %q UID changed from %q to %q", ErrStateConflict, name, uid, object.GetUID())
		}
		if object.GetDeletionTimestamp() == nil {
			return fmt.Errorf("%w: ShiftPVPool %q is not deleting", ErrStateConflict, name)
		}
		annotations := object.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		if annotations[PoolIdentityReleaseAnnotation] == uid {
			return nil
		}
		annotations[PoolIdentityReleaseAnnotation] = uid
		object.SetAnnotations(annotations)
		if _, err := resource.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("approve ShiftPVPool identity release: %w", err)
		}
		return nil
	})
}

func (r *Registry) updatePoolFinalizer(ctx context.Context, name, uid string, present bool) error {
	if err := r.validate(); err != nil {
		return err
	}
	if name == "" || uid == "" {
		return fmt.Errorf("ShiftPVPool name and UID are required")
	}
	resource := r.Client.Resource(PoolResource)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		object, err := resource.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && !present {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read ShiftPVPool finalizer: %w", err)
		}
		if string(object.GetUID()) != uid {
			return fmt.Errorf("%w: ShiftPVPool %q UID changed from %q to %q", ErrStateConflict, name, uid, object.GetUID())
		}
		finalizers := object.GetFinalizers()
		hasFinalizer := slices.Contains(finalizers, PoolProtectionFinalizer)
		if present == hasFinalizer {
			return nil
		}
		if present {
			if object.GetDeletionTimestamp() != nil {
				return fmt.Errorf("%w: ShiftPVPool %q is already deleting without protection", ErrStateConflict, name)
			}
			finalizers = append(finalizers, PoolProtectionFinalizer)
		} else {
			filtered := finalizers[:0]
			for _, finalizer := range finalizers {
				if finalizer != PoolProtectionFinalizer {
					filtered = append(filtered, finalizer)
				}
			}
			finalizers = filtered
		}
		object.SetFinalizers(finalizers)
		if _, err := resource.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update ShiftPVPool finalizer: %w", err)
		}
		return nil
	})
}

func (p Pool) ReadyAt(now time.Time, staleAfter time.Duration) (bool, string) {
	if p.DeletionTimestamp != nil {
		return false, "PoolDeregistering"
	}
	condition := meta.FindStatusCondition(p.Status.Conditions, PoolConditionReady)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		if condition != nil && condition.Reason != "" {
			return false, condition.Reason
		}
		return false, "ProbePending"
	}
	if p.Status.ObservedGeneration != p.Generation || condition.ObservedGeneration != p.Generation {
		return false, "ProbeOutdated"
	}
	if p.Status.LastProbeTime.IsZero() || staleAfter <= 0 || now.Sub(p.Status.LastProbeTime.Time) > staleAfter || now.Before(p.Status.LastProbeTime.Time) {
		return false, "ProbeStale"
	}
	return true, condition.Reason
}

// CleanupReadyAt keeps an exact, already-approved cleanup executable while a
// Pool is terminating. New placement continues to use ReadyAt and remains
// closed for the same Pool.
func (p Pool) CleanupReadyAt(now time.Time, staleAfter time.Duration) (bool, string) {
	if p.DeletionTimestamp == nil {
		return p.ReadyAt(now, staleAfter)
	}
	if p.Status.ObservedGeneration != p.Generation {
		return false, "ProbeOutdated"
	}
	for _, conditionType := range []string{PoolConditionAccessible, PoolConditionWritable, PoolConditionCapacityReadable} {
		condition := meta.FindStatusCondition(p.Status.Conditions, conditionType)
		if condition == nil {
			return false, "ProbePending"
		}
		if condition.ObservedGeneration != p.Generation {
			return false, "ProbeOutdated"
		}
		if condition.Status != metav1.ConditionTrue {
			if condition.Reason != "" {
				return false, condition.Reason
			}
			return false, "ProbeFailed"
		}
	}
	if p.Status.LastProbeTime.IsZero() || staleAfter <= 0 || now.Sub(p.Status.LastProbeTime.Time) > staleAfter || now.Before(p.Status.LastProbeTime.Time) {
		return false, "ProbeStale"
	}
	return true, "PoolCleanupReady"
}

func poolFrom(object *unstructured.Unstructured) (Pool, error) {
	status := PoolStatus{}
	if data, found, err := unstructured.NestedMap(object.Object, "status"); err != nil {
		return Pool{}, fmt.Errorf("decode ShiftPVPool %q status: %w", object.GetName(), err)
	} else if found {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(data, &status); err != nil {
			return Pool{}, fmt.Errorf("decode ShiftPVPool %q status: %w", object.GetName(), err)
		}
	}
	nodeName, _, _ := unstructured.NestedString(object.Object, "spec", "nodeName")
	mountPath, _, _ := unstructured.NestedString(object.Object, "spec", "mountPath")
	capacityLimit, _, _ := unstructured.NestedString(object.Object, "spec", "capacity", "limit")
	return Pool{
		Name: object.GetName(), UID: string(object.GetUID()), NodeName: nodeName, MountPath: filepath.Clean(mountPath),
		CapacityLimit: capacityLimit, Generation: object.GetGeneration(), DeletionTimestamp: object.GetDeletionTimestamp(),
		Finalizers: append([]string(nil), object.GetFinalizers()...), IdentityReleaseApproval: object.GetAnnotations()[PoolIdentityReleaseAnnotation], Status: status,
	}, nil
}

func (r *Registry) CreateMove(ctx context.Context, generateName string, spec MoveSpec) (Move, error) {
	if err := r.validate(); err != nil {
		return Move{}, err
	}
	object, err := r.Client.Resource(MoveResource).Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "shiftpv.io/v1alpha1",
		"kind":       "ShiftPVMove",
		"metadata":   map[string]any{"generateName": generateName},
		"spec":       map[string]any{"volumeID": spec.VolumeID, "sourceNode": spec.SourceNode},
	}}, metav1.CreateOptions{})
	if err != nil {
		return Move{}, fmt.Errorf("create ShiftPVMove: %w", err)
	}
	return moveFrom(object)
}

func (r *Registry) DeleteMove(ctx context.Context, name, uid string) error {
	if err := r.validate(); err != nil {
		return err
	}
	if name == "" || uid == "" {
		return fmt.Errorf("ShiftPVMove name and UID are required for deletion")
	}
	precondition := types.UID(uid)
	err := r.Client.Resource(MoveResource).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &precondition},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ShiftPVMove: %w", err)
	}
	return nil
}

func (r *Registry) GetMove(ctx context.Context, name string) (Move, error) {
	if err := r.validate(); err != nil {
		return Move{}, err
	}
	object, err := r.Client.Resource(MoveResource).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Move{}, fmt.Errorf("get ShiftPVMove: %w", err)
	}
	return moveFrom(object)
}

func (r *Registry) ListMoves(ctx context.Context) ([]Move, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	list, err := r.Client.Resource(MoveResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ShiftPVMove: %w", err)
	}
	result := make([]Move, 0, len(list.Items))
	for index := range list.Items {
		move, moveErr := moveFrom(&list.Items[index])
		if moveErr != nil {
			return nil, fmt.Errorf("decode ShiftPVMove %q: %w", list.Items[index].GetName(), moveErr)
		}
		result = append(result, move)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func (r *Registry) SetMoveStatus(ctx context.Context, name, uid string, status MoveStatus) error {
	if err := r.validate(); err != nil {
		return err
	}
	if name == "" || uid == "" {
		return fmt.Errorf("ShiftPVMove name and UID are required for status update")
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource := r.Client.Resource(MoveResource)
		object, err := resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get ShiftPVMove for status update: %w", err)
		}
		if string(object.GetUID()) != uid {
			return fmt.Errorf("%w: ShiftPVMove %q UID changed from %q to %q", ErrStateConflict, name, uid, object.GetUID())
		}
		current, err := moveStatusFrom(object)
		if err != nil {
			return err
		}
		if err := preserveMoveIdentity(current, status); err != nil {
			return err
		}
		setMoveStatus(object, status)
		if _, err := resource.UpdateStatus(ctx, object, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update ShiftPVMove status: %w", err)
		}
		return nil
	})
}

func (r *Registry) mutateState(ctx context.Context, volumeID string, mutate func(State) (State, error)) error {
	if err := r.validate(); err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource := r.Client.Resource(VolumeResource)
		object, err := resource.Get(ctx, volumeID, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get ShiftPVVolume for status update: %w", err)
		}
		current, err := stateFrom(object)
		if err != nil {
			return err
		}
		next, err := mutate(current)
		if err != nil {
			return err
		}
		if next.CreationOperationID == "" {
			next.CreationOperationID = current.CreationOperationID
		}
		if next.DeletionOperationID == "" {
			next.DeletionOperationID = current.DeletionOperationID
		}
		if next.CurrentCopy == nil {
			next.CurrentCopy = current.CurrentCopy
		}
		setState(object, next)
		if _, err := resource.UpdateStatus(ctx, object, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update ShiftPVVolume status: %w", err)
		}
		return nil
	})
}

func (r *Registry) getVolume(ctx context.Context, volumeID string) (*unstructured.Unstructured, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	object, err := r.Client.Resource(VolumeResource).Get(ctx, volumeID, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get ShiftPVVolume: %w", err)
	}
	return object, nil
}

func (r *Registry) validate() error {
	if r == nil || r.Client == nil {
		return fmt.Errorf("volume registry is not configured")
	}
	return nil
}

func stateFrom(object *unstructured.Unstructured) (State, error) {
	phase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
	ownerNode, _, _ := unstructured.NestedString(object.Object, "status", "ownerNode")
	activeMove, _, _ := unstructured.NestedString(object.Object, "status", "activeMove")
	publishedNodes, _, err := unstructured.NestedStringSlice(object.Object, "status", "publishedNodes")
	if err != nil {
		return State{}, fmt.Errorf("decode publishedNodes: %w", err)
	}
	creationOperationID, _, _ := unstructured.NestedString(object.Object, "status", "creationOperationID")
	deletionOperationID, _, _ := unstructured.NestedString(object.Object, "status", "deletionOperationID")
	var currentCopy *volume.CopyIdentity
	if data, found, nestedErr := unstructured.NestedMap(object.Object, "status", "currentCopy"); nestedErr != nil {
		return State{}, nestedErr
	} else if found {
		var copy volume.CopyIdentity
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(data, &copy); err != nil {
			return State{}, fmt.Errorf("decode current copy: %w", err)
		}
		if err := copy.Validate(); err != nil {
			return State{}, err
		}
		currentCopy = &copy
	}
	return State{
		UID: string(object.GetUID()), Phase: phase, OwnerNode: ownerNode, ActiveMove: activeMove,
		PublishedNodes: publishedNodes, CreationOperationID: creationOperationID, DeletionOperationID: deletionOperationID, CurrentCopy: currentCopy,
	}, nil
}

func setState(object *unstructured.Unstructured, state State) {
	object.Object["status"] = map[string]any{
		"phase": state.Phase, "ownerNode": state.OwnerNode, "activeMove": state.ActiveMove,
		"publishedNodes": stringSliceToAny(state.PublishedNodes),
	}
	status := object.Object["status"].(map[string]any)
	if state.CreationOperationID != "" {
		status["creationOperationID"] = state.CreationOperationID
	}
	if state.DeletionOperationID != "" {
		status["deletionOperationID"] = state.DeletionOperationID
	}
	if state.CurrentCopy != nil {
		if encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(state.CurrentCopy); err == nil {
			status["currentCopy"] = encoded
		}
	}
}

func moveFrom(object *unstructured.Unstructured) (Move, error) {
	volumeID, _, _ := unstructured.NestedString(object.Object, "spec", "volumeID")
	sourceNode, _, _ := unstructured.NestedString(object.Object, "spec", "sourceNode")
	recovery, _, _ := unstructured.NestedString(object.Object, "spec", "recovery")
	status, err := moveStatusFrom(object)
	if err != nil {
		return Move{}, err
	}
	return Move{Name: object.GetName(), UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion(), Spec: MoveSpec{VolumeID: volumeID, SourceNode: sourceNode, Recovery: recovery}, Status: status}, nil
}

func moveStatusFrom(object *unstructured.Unstructured) (MoveStatus, error) {
	read := func(name string) string {
		value, _, _ := unstructured.NestedString(object.Object, "status", name)
		return value
	}
	candidates, _, err := unstructured.NestedStringSlice(object.Object, "status", "candidateNodes")
	if err != nil {
		return MoveStatus{}, fmt.Errorf("decode candidateNodes: %w", err)
	}
	evictionRequested, _, _ := unstructured.NestedBool(object.Object, "status", "evictionRequested")
	sourceBytes, _, _ := unstructured.NestedInt64(object.Object, "status", "sourceBytes")
	capacityApproved, _, _ := unstructured.NestedBool(object.Object, "status", "capacityApproved")
	readCopy := func(name string) (*volume.CopyIdentity, error) {
		data, found, nestedErr := unstructured.NestedMap(object.Object, "status", name)
		if nestedErr != nil || !found {
			return nil, nestedErr
		}
		var identity volume.CopyIdentity
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(data, &identity); err != nil {
			return nil, err
		}
		if err := identity.Validate(); err != nil {
			return nil, err
		}
		return &identity, nil
	}
	sourceCopy, err := readCopy("sourceCopy")
	if err != nil {
		return MoveStatus{}, fmt.Errorf("decode sourceCopy: %w", err)
	}
	incomingCopy, err := readCopy("incomingCopy")
	if err != nil {
		return MoveStatus{}, fmt.Errorf("decode incomingCopy: %w", err)
	}
	destinationCopy, err := readCopy("destinationCopy")
	if err != nil {
		return MoveStatus{}, fmt.Errorf("decode destinationCopy: %w", err)
	}
	return MoveStatus{
		Phase: read("phase"), Reason: read("reason"), Message: read("message"),
		LastTransitionTime: read("lastTransitionTime"), LastProgressTime: read("lastProgressTime"),
		PersistentVolumeName: read("persistentVolumeName"), ClaimNamespace: read("persistentVolumeClaimNamespace"),
		ClaimName: read("persistentVolumeClaimName"), ConsumerName: read("consumerName"), ConsumerUID: read("consumerUID"), ReplacementName: read("replacementName"),
		ReplacementUID:  read("replacementUID"),
		DestinationNode: read("destinationNode"), DestinationPoolUID: read("destinationPoolUID"), SourceBytes: sourceBytes, CapacityApproved: capacityApproved,
		CapacityReason: read("capacityReason"), CandidateNodes: candidates, EvictionRequested: evictionRequested,
		CopyJobName: read("copyJobName"), PromotionJobName: read("promotionJobName"), CleanupJobName: read("cleanupJobName"),
		CopyOperationID: read("copyOperationID"), PromotionOperationID: read("promotionOperationID"), CleanupName: read("cleanupName"),
		SourceCopy: sourceCopy, IncomingCopy: incomingCopy, DestinationCopy: destinationCopy,
		RecoveryPhase: read("recoveryPhase"), RecoveryOwner: read("recoveryOwner"),
		RecoveryReason: read("recoveryReason"), RecoveryMessage: read("recoveryMessage"),
	}, nil
}

func setMoveStatus(object *unstructured.Unstructured, status MoveStatus) {
	object.Object["status"] = map[string]any{
		"phase": status.Phase, "reason": status.Reason, "message": status.Message,
		"lastTransitionTime": status.LastTransitionTime, "lastProgressTime": status.LastProgressTime,
		"persistentVolumeName": status.PersistentVolumeName, "persistentVolumeClaimNamespace": status.ClaimNamespace,
		"persistentVolumeClaimName": status.ClaimName, "consumerName": status.ConsumerName, "consumerUID": status.ConsumerUID, "replacementName": status.ReplacementName,
		"replacementUID":  status.ReplacementUID,
		"destinationNode": status.DestinationNode, "destinationPoolUID": status.DestinationPoolUID, "sourceBytes": status.SourceBytes,
		"capacityApproved": status.CapacityApproved, "capacityReason": status.CapacityReason,
		"candidateNodes":    stringSliceToAny(status.CandidateNodes),
		"evictionRequested": status.EvictionRequested, "copyJobName": status.CopyJobName,
		"promotionJobName": status.PromotionJobName, "cleanupJobName": status.CleanupJobName,
		"copyOperationID": status.CopyOperationID, "promotionOperationID": status.PromotionOperationID, "cleanupName": status.CleanupName,
		"recoveryPhase": status.RecoveryPhase, "recoveryOwner": status.RecoveryOwner,
		"recoveryReason": status.RecoveryReason, "recoveryMessage": status.RecoveryMessage,
	}
	encoded := object.Object["status"].(map[string]any)
	for name, identity := range map[string]*volume.CopyIdentity{"sourceCopy": status.SourceCopy, "incomingCopy": status.IncomingCopy, "destinationCopy": status.DestinationCopy} {
		if identity == nil {
			continue
		}
		if value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(identity); err == nil {
			encoded[name] = value
		}
	}
}

func preserveMoveIdentity(current, next MoveStatus) error {
	for _, item := range []struct {
		name          string
		current, next *volume.CopyIdentity
	}{
		{"sourceCopy", current.SourceCopy, next.SourceCopy},
		{"incomingCopy", current.IncomingCopy, next.IncomingCopy},
		{"destinationCopy", current.DestinationCopy, next.DestinationCopy},
	} {
		if item.current != nil && (item.next == nil || *item.current != *item.next) {
			return fmt.Errorf("%w: Move %s is immutable", ErrStateConflict, item.name)
		}
	}
	for _, item := range []struct{ name, current, next string }{
		{"copyOperationID", current.CopyOperationID, next.CopyOperationID},
		{"promotionOperationID", current.PromotionOperationID, next.PromotionOperationID},
	} {
		if item.current != "" && item.current != item.next {
			return fmt.Errorf("%w: Move %s is immutable", ErrStateConflict, item.name)
		}
	}
	return nil
}

func stringSliceToAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}
