package volumeapi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
)

var (
	VolumeResource = schema.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvvolumes"}
	PoolResource   = schema.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvpools"}
	MoveResource   = schema.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvmoves"}

	ErrStateConflict     = errors.New("ShiftPV state precondition failed")
	ErrPoolConfiguration = errors.New("ShiftPV Pool configuration is invalid")
	ErrPoolNotFound      = errors.New("ShiftPV Pool is not registered")
	ErrPoolNotReady      = errors.New("ShiftPV Pool is not ready")
)

const (
	PoolConditionReady      = "Ready"
	PoolConditionAccessible = "Accessible"
	// PoolConditionMounted is retained so newer node plugins can remove the
	// obsolete condition written by releases that required an exact mount point.
	PoolConditionMounted           = "Mounted"
	PoolConditionWritable          = "Writable"
	PoolConditionCapacityReadable  = "CapacityReadable"
	DefaultPoolReadinessStaleAfter = 3 * time.Minute
)

const (
	PhaseReady   = "Ready"
	PhaseMoving  = "Moving"
	PhaseBlocked = "Blocked"
)

type State struct {
	Phase          string
	OwnerNode      string
	ActiveMove     string
	PublishedNodes []string
}

type Pool struct {
	Name          string
	NodeName      string
	MountPath     string
	CapacityLimit string
	Generation    int64
	Status        PoolStatus
}

type PoolStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastProbeTime      metav1.Time        `json:"lastProbeTime,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
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
	SourceBytes          int64
	CapacityApproved     bool
	CapacityReason       string
	CandidateNodes       []string
	EvictionRequested    bool
	CopyJobName          string
	PromotionJobName     string
	CleanupJobName       string
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

func (r *Registry) Delete(ctx context.Context, volumeID string) error {
	if err := r.validate(); err != nil {
		return err
	}
	err := r.Client.Resource(VolumeResource).Delete(ctx, volumeID, metav1.DeleteOptions{})
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
		if current.Phase != expectedPhase || current.ActiveMove != expectedActiveMove || current.OwnerNode != expectedOwner {
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
		if ok, _ := pool.ReadyAt(now, staleAfter); ok {
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
	return pool, nil
}

func (r *Registry) SetPoolStatus(ctx context.Context, name, nodeName string, status PoolStatus) error {
	if err := r.validate(); err != nil {
		return err
	}
	if name == "" || nodeName == "" {
		return fmt.Errorf("ShiftPVPool name and node name are required")
	}
	resource := r.Client.Resource(PoolResource)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		object, err := resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read ShiftPVPool status: %w", err)
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

func (p Pool) ReadyAt(now time.Time, staleAfter time.Duration) (bool, string) {
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
		Name: object.GetName(), NodeName: nodeName, MountPath: filepath.Clean(mountPath),
		CapacityLimit: capacityLimit, Generation: object.GetGeneration(), Status: status,
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

func (r *Registry) SetMoveStatus(ctx context.Context, name string, status MoveStatus) error {
	if err := r.validate(); err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource := r.Client.Resource(MoveResource)
		object, err := resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get ShiftPVMove for status update: %w", err)
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
	return State{Phase: phase, OwnerNode: ownerNode, ActiveMove: activeMove, PublishedNodes: publishedNodes}, nil
}

func setState(object *unstructured.Unstructured, state State) {
	object.Object["status"] = map[string]any{
		"phase": state.Phase, "ownerNode": state.OwnerNode, "activeMove": state.ActiveMove,
		"publishedNodes": stringSliceToAny(state.PublishedNodes),
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
	return MoveStatus{
		Phase: read("phase"), Reason: read("reason"), Message: read("message"),
		LastTransitionTime: read("lastTransitionTime"), LastProgressTime: read("lastProgressTime"),
		PersistentVolumeName: read("persistentVolumeName"), ClaimNamespace: read("persistentVolumeClaimNamespace"),
		ClaimName: read("persistentVolumeClaimName"), ConsumerName: read("consumerName"), ConsumerUID: read("consumerUID"), ReplacementName: read("replacementName"),
		ReplacementUID:  read("replacementUID"),
		DestinationNode: read("destinationNode"), SourceBytes: sourceBytes, CapacityApproved: capacityApproved,
		CapacityReason: read("capacityReason"), CandidateNodes: candidates, EvictionRequested: evictionRequested,
		CopyJobName: read("copyJobName"), PromotionJobName: read("promotionJobName"), CleanupJobName: read("cleanupJobName"),
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
		"destinationNode": status.DestinationNode, "sourceBytes": status.SourceBytes,
		"capacityApproved": status.CapacityApproved, "capacityReason": status.CapacityReason,
		"candidateNodes":    stringSliceToAny(status.CandidateNodes),
		"evictionRequested": status.EvictionRequested, "copyJobName": status.CopyJobName,
		"promotionJobName": status.PromotionJobName, "cleanupJobName": status.CleanupJobName,
		"recoveryPhase": status.RecoveryPhase, "recoveryOwner": status.RecoveryOwner,
		"recoveryReason": status.RecoveryReason, "recoveryMessage": status.RecoveryMessage,
	}
}

func stringSliceToAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}
