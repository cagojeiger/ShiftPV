package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type memoryRepository struct {
	volumes              map[string]volumeapi.State
	pools                []volumeapi.Pool
	readyPools           []volumeapi.Pool
	readyPoolsConfigured bool
	moves                []volumeapi.Move
}

type countingRepository struct {
	memoryRepository
	listVolumeCalls atomic.Int32
}

type rawGetRepository struct {
	*memoryRepository
	state volumeapi.State
}

func (r *rawGetRepository) Get(context.Context, string) (volumeapi.State, error) {
	return r.state, nil
}

func (c *countingRepository) ListVolumes(ctx context.Context) (map[string]volumeapi.State, error) {
	c.listVolumeCalls.Add(1)
	return c.memoryRepository.ListVolumes(ctx)
}

func (m *memoryRepository) ListVolumes(context.Context) (map[string]volumeapi.State, error) {
	volumes := make(map[string]volumeapi.State, len(m.volumes))
	for id, state := range m.volumes {
		volumes[id] = identifiedTestState(id, state, m.pools)
	}
	return volumes, nil
}
func (m *memoryRepository) Get(_ context.Context, id string) (volumeapi.State, error) {
	state, exists := m.volumes[id]
	if !exists {
		return volumeapi.State{}, fmt.Errorf("volume not found")
	}
	return identifiedTestState(id, state, m.pools), nil
}
func (m *memoryRepository) CompareAndSetState(_ context.Context, id, phase, active, owner string, next volumeapi.State) error {
	current := identifiedTestState(id, m.volumes[id], m.pools)
	if next.UID == "" || current.UID != next.UID || current.Phase != phase || current.ActiveMove != active || current.OwnerNode != owner {
		return volumeapi.ErrStateConflict
	}
	next.PublishedNodes = current.PublishedNodes
	if next.CreationOperationID == "" {
		next.CreationOperationID = current.CreationOperationID
	}
	if next.DeletionOperationID == "" {
		next.DeletionOperationID = current.DeletionOperationID
	}
	if next.CurrentCopy == nil {
		next.CurrentCopy = current.CurrentCopy
	}
	if next.Phase == volumeapi.PhaseReady {
		for _, node := range current.PublishedNodes {
			if node != next.OwnerNode {
				return volumeapi.ErrStateConflict
			}
		}
	}
	m.volumes[id] = next
	return nil
}
func (m *memoryRepository) Pools(context.Context) ([]volumeapi.Pool, error) {
	return identifiedTestPools(m.pools), nil
}
func (m *memoryRepository) ReadyPools(context.Context) ([]volumeapi.Pool, error) {
	if m.readyPoolsConfigured {
		return identifiedReadyTestPools(m.readyPools, m.volumes, m.pools), nil
	}
	return identifiedReadyTestPools(m.pools, m.volumes, m.pools), nil
}
func (m *memoryRepository) CreateMove(_ context.Context, _ string, spec volumeapi.MoveSpec) (volumeapi.Move, error) {
	move := volumeapi.Move{Name: "move-generated", UID: "uid", Spec: spec}
	m.moves = append(m.moves, move)
	return move, nil
}
func (m *memoryRepository) DeleteMove(_ context.Context, name, uid string) error {
	for index := range m.moves {
		if m.moves[index].Name != name {
			continue
		}
		if string(m.moves[index].UID) != uid {
			return volumeapi.ErrStateConflict
		}
		m.moves = append(m.moves[:index], m.moves[index+1:]...)
		return nil
	}
	return nil
}
func (m *memoryRepository) ListMoves(context.Context) ([]volumeapi.Move, error) { return m.moves, nil }
func (m *memoryRepository) SetMoveStatus(_ context.Context, name, uid string, status volumeapi.MoveStatus) error {
	for index := range m.moves {
		if m.moves[index].Name == name {
			if m.moves[index].UID != uid {
				return volumeapi.ErrStateConflict
			}
			m.moves[index].Status = status
			return nil
		}
	}
	return fmt.Errorf("move not found")
}

func identifiedTestPools(pools []volumeapi.Pool) []volumeapi.Pool {
	result := append([]volumeapi.Pool(nil), pools...)
	for index := range result {
		if result[index].Name == "" {
			result[index].Name = result[index].NodeName + "-pool"
		}
		if result[index].UID == "" {
			result[index].UID = result[index].Name + "-uid"
		}
	}
	return result
}

func identifiedReadyTestPools(pools []volumeapi.Pool, states map[string]volumeapi.State, registered []volumeapi.Pool) []volumeapi.Pool {
	result := identifiedTestPools(pools)
	for index := range result {
		if result[index].Status.Inventory != nil {
			continue
		}
		inventory := &volumeapi.PoolInventory{Valid: true}
		for id, raw := range states {
			state := identifiedTestState(id, raw, registered)
			if state.CurrentCopy == nil || state.CurrentCopy.PoolName != result[index].Name || state.CurrentCopy.PoolUID != result[index].UID {
				continue
			}
			copy := *state.CurrentCopy
			inventory.Copies = append(inventory.Copies, volumeapi.CopyObservation{Marker: "test-copy", Identity: &copy, Present: true})
		}
		result[index].Status.Inventory = inventory
	}
	return result
}

func identifiedTestState(id string, state volumeapi.State, pools []volumeapi.Pool) volumeapi.State {
	if state.UID == "" {
		state.UID = "test-volume-uid"
	}
	if state.CurrentCopy != nil || state.OwnerNode == "" {
		return state
	}
	for _, pool := range identifiedTestPools(pools) {
		if pool.NodeName != state.OwnerNode {
			continue
		}
		state.CurrentCopy = &volume.CopyIdentity{
			InstallationID: "test-installation", PoolName: pool.Name, PoolUID: pool.UID,
			VolumeID: id, VolumeUID: state.UID, CopyID: "test-copy-" + state.OwnerNode,
			NodeName: state.OwnerNode, Role: volume.RoleServing,
		}
		break
	}
	return state
}

func TestDiscoverMovesCreatesOneMoveForHealthyCordon(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "source", PublishedNodes: []string{"source"}}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/pool"}, {Name: "destination", NodeName: "destination", MountPath: "/pool"}},
	}
	client := fake.NewSimpleClientset(mobilityObjects(volumeID)...)
	reconciler := &Reconciler{
		Client: client, Repository: repository, Namespace: "system", HelperImage: "helper", ServiceAccountName: "shiftpv-controller",
		Cleanups: newTestCleanupStore(), CleanupOperator: receiptCleanupOperator{},
	}
	if err := reconciler.discoverMoves(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.moves) != 1 || repository.moves[0].Status.Phase != string(fsm.PhasePending) {
		t.Fatalf("moves = %#v", repository.moves)
	}
	if err := reconciler.discoverMoves(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.moves) != 1 {
		t.Fatalf("duplicate moves were created: %#v", repository.moves)
	}
}

func TestDiscoverMovesSkipsPoolThatIsNotReady(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	source := volumeapi.Pool{Name: "source", NodeName: "source", MountPath: "/pool"}
	destination := volumeapi.Pool{Name: "destination", NodeName: "destination", MountPath: "/pool"}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "source", PublishedNodes: []string{"source"}}},
		pools:   []volumeapi.Pool{source, destination}, readyPools: []volumeapi.Pool{source}, readyPoolsConfigured: true,
	}
	reconciler := &Reconciler{Client: fake.NewSimpleClientset(mobilityObjects(volumeID)...), Repository: repository, Namespace: "system", HelperImage: "helper"}
	if err := reconciler.discoverMoves(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.moves) != 0 {
		t.Fatalf("created move with no Ready destination: %#v", repository.moves)
	}
}

func TestDiscoverMovesSkipsDestinationContainingServingCopy(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	source := volumeapi.Pool{Name: "source", UID: "source-pool-uid", NodeName: "source", MountPath: "/pool"}
	destination := volumeapi.Pool{Name: "destination", UID: "destination-pool-uid", NodeName: "destination", MountPath: "/pool"}
	state := identifiedTestState(volumeID, volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: "source", PublishedNodes: []string{"source"}}, []volumeapi.Pool{source, destination})
	foreign := *state.CurrentCopy
	foreign.PoolName, foreign.PoolUID, foreign.NodeName, foreign.CopyID = destination.Name, destination.UID, destination.NodeName, "old-destination-copy"
	source.Status.Inventory = &volumeapi.PoolInventory{Valid: true, Copies: []volumeapi.CopyObservation{{Identity: state.CurrentCopy, Present: true}}}
	destination.Status.Inventory = &volumeapi.PoolInventory{Valid: true, Copies: []volumeapi.CopyObservation{{Identity: &foreign, Present: true}}}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: state}, pools: []volumeapi.Pool{source, destination},
		readyPools: []volumeapi.Pool{source, destination}, readyPoolsConfigured: true,
	}
	client := fake.NewSimpleClientset(mobilityObjects(volumeID)...)
	reconciler := &Reconciler{Client: client, Repository: repository, Namespace: "system", HelperImage: "helper"}
	if err := reconciler.discoverMoves(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.moves) != 0 || repository.volumes[volumeID].Phase != volumeapi.PhaseReady {
		t.Fatalf("occupied destination admitted: moves=%#v volume=%#v", repository.moves, repository.volumes[volumeID])
	}
	assertNoEviction(t, client)
}

func TestDiscoverMovesSkipsIneligibleVolumes(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	for name, mutate := range map[string]func([]runtime.Object, *memoryRepository) []runtime.Object{
		"namespace not opted in": func(objects []runtime.Object, _ *memoryRepository) []runtime.Object {
			objects[0].(*corev1.Namespace).Labels = nil
			return objects
		},
		"consumer missing": func(objects []runtime.Object, _ *memoryRepository) []runtime.Object {
			return objects[:len(objects)-1]
		},
		"bare pod": func(objects []runtime.Object, _ *memoryRepository) []runtime.Object {
			objects[len(objects)-1].(*corev1.Pod).OwnerReferences = nil
			return objects
		},
		"destination missing": func(objects []runtime.Object, repository *memoryRepository) []runtime.Object {
			repository.pools = repository.pools[:1]
			return objects
		},
	} {
		t.Run(name, func(t *testing.T) {
			repository := &memoryRepository{
				volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "source", PublishedNodes: []string{"source"}}},
				pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/pool"}, {Name: "destination", NodeName: "destination", MountPath: "/pool"}},
			}
			objects := mutate(mobilityObjects(volumeID), repository)
			reconciler := &Reconciler{Client: fake.NewSimpleClientset(objects...), Repository: repository, Namespace: "system", HelperImage: "helper"}
			if err := reconciler.discoverMoves(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(repository.moves) != 0 || repository.volumes[volumeID].Phase != volumeapi.PhaseReady {
				t.Fatalf("moves=%#v volume=%#v", repository.moves, repository.volumes[volumeID])
			}
		})
	}
}

func TestBindingLossAfterLockBlocksWithoutPanic(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"}, Status: volumeapi.MoveStatus{Phase: string(fsm.PhaseLocking)}}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: move.Name}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/pool"}, {Name: "destination", NodeName: "destination", MountPath: "/pool"}},
		moves:   []volumeapi.Move{move},
	}
	client := fake.NewSimpleClientset(readyNode("source", true), readyNode("destination", false))
	reconciler := &Reconciler{Client: client, Repository: repository, Namespace: "system", HelperImage: "helper"}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.moves[0].Status.Phase != string(fsm.PhaseBlocked) || repository.moves[0].Status.Reason != "VolumeBindingMissing" {
		t.Fatalf("move = %#v", repository.moves[0])
	}
	if repository.volumes[volumeID].Phase != volumeapi.PhaseBlocked {
		t.Fatalf("volume = %#v", repository.volumes[volumeID])
	}
}

func TestPendingMoveIsCancelledWhenSourceWasUncordonedBeforeLock(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{
		Name: "move-test", UID: "move-uid",
		Spec:   volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{Phase: string(fsm.PhasePending)},
	}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "source"}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/pool"}, {Name: "destination", NodeName: "destination", MountPath: "/pool"}},
		moves:   []volumeapi.Move{move},
	}
	// The binding has already disappeared with its namespace. Since the source
	// is now schedulable and no lock was taken, this is an obsolete discovery
	// transaction rather than a VolumeBindingMissing safety failure.
	client := fake.NewSimpleClientset(readyNode("source", false), readyNode("destination", false))
	reconciler := &Reconciler{Client: client, Repository: repository, Namespace: "system", HelperImage: "helper"}

	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.moves) != 0 {
		t.Fatalf("obsolete pre-lock move remained: %#v", repository.moves)
	}
	state := repository.volumes[volumeID]
	if state.Phase != volumeapi.PhaseReady || state.ActiveMove != "" || state.OwnerNode != "source" {
		t.Fatalf("obsolete move cancellation changed volume authority: %#v", state)
	}
}

func TestObserveRecognizesOwnerCommitBeforeMovePhasePersistence(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	_, _, destination := testCopyIdentities(volumeID, "source", "destination")
	move := volumeapi.Move{
		Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{Phase: string(fsm.PhaseCommitting), ConsumerName: "consumer", DestinationNode: "destination", DestinationPoolUID: destination.PoolUID, DestinationCopy: &destination},
	}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {UID: destination.VolumeUID, Phase: volumeapi.PhaseReady, OwnerNode: "destination", ActiveMove: move.Name, CurrentCopy: &destination}},
		pools:   []volumeapi.Pool{{Name: "source", UID: "source-pool-uid", NodeName: "source", MountPath: "/source-pool"}, {Name: "destination", UID: "destination-pool-uid", NodeName: "destination", MountPath: "/destination-pool"}},
		moves:   []volumeapi.Move{move},
	}
	reconciler := &Reconciler{Client: fake.NewSimpleClientset(mobilityObjects(volumeID)...), Repository: repository, Namespace: "system", HelperImage: "helper"}
	observed, err := reconciler.observe(context.Background(), move)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.FSM.OwnerCommitted || observed.FSM.SourceAuthorityInvalid || observed.DestinationNode != "destination" {
		t.Fatalf("committed observation = %#v", observed)
	}
	for name, current := range map[string]*volume.CopyIdentity{
		"missing current copy": nil,
		"different current copy": func() *volume.CopyIdentity {
			changed := destination
			changed.CopyID = "different-copy"
			return &changed
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			state := repository.volumes[volumeID]
			state.CurrentCopy = current
			reconciler.Repository = &rawGetRepository{memoryRepository: repository, state: state}
			observed, err := reconciler.observe(context.Background(), move)
			if err != nil {
				t.Fatal(err)
			}
			if observed.FSM.OwnerCommitted {
				t.Fatalf("owner commit accepted without exact destination authority: %#v", observed.Volume)
			}
		})
	}
}

func TestObserveMarksSelectedNotReadyDestinationUnavailable(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{
		Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{
			Phase: string(fsm.PhaseCopying), ConsumerName: "consumer",
			CandidateNodes: []string{"destination"}, DestinationNode: "destination",
		},
	}
	source := volumeapi.Pool{Name: "source", NodeName: "source", MountPath: "/source-pool"}
	destination := volumeapi.Pool{Name: "destination", NodeName: "destination", MountPath: "/destination-pool"}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: move.Name}},
		pools:   []volumeapi.Pool{source, destination}, readyPools: []volumeapi.Pool{source}, readyPoolsConfigured: true,
		moves: []volumeapi.Move{move},
	}
	objects := mobilityObjects(volumeID)
	reconciler := &Reconciler{Client: fake.NewSimpleClientset(objects...), Repository: repository, Namespace: "system", HelperImage: "helper"}
	observed, err := reconciler.observe(context.Background(), move)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.FSM.DestinationUnavailable || observed.FSM.DestinationBlocked || observed.DestinationNode != "destination" {
		t.Fatalf("destination observation = %#v", observed)
	}
}

func TestObserveAllowsOnlyActiveMoveToRepairDestinationCrashWindow(t *testing.T) {
	now := time.Date(2026, 9, 12, 4, 0, 0, 0, time.UTC)
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	sourceCopy, incoming, destinationCopy := testCopyIdentities(volumeID, "source", "destination")
	incoming.CopyID = "move-move-uid-incoming"
	destinationCopy.CopyID = "move-move-uid-serving"
	baseMove := volumeapi.Move{
		Name: "move-test", UID: "move-uid", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{
			ConsumerName: "old-consumer", ConsumerUID: "old-consumer-uid", CandidateNodes: []string{"destination"},
			DestinationNode: "destination", DestinationPoolUID: destinationCopy.PoolUID, CapacityApproved: true, SourceBytes: 1,
			SourceCopy: &sourceCopy, IncomingCopy: &incoming, DestinationCopy: &destinationCopy,
			CopyOperationID: "copy-move-uid", PromotionOperationID: "promote-move-uid",
		},
	}
	source := volumeapi.Pool{
		Name: sourceCopy.PoolName, UID: sourceCopy.PoolUID, NodeName: sourceCopy.NodeName, MountPath: "/source-pool",
		Status: volumeapi.PoolStatus{Inventory: &volumeapi.PoolInventory{Valid: true, Copies: []volumeapi.CopyObservation{{Identity: &sourceCopy, Present: true}}}},
	}
	readyStatus := volumeapi.PoolStatus{
		ObservedGeneration: 1, LastProbeTime: metav1.NewTime(now),
		Conditions: []metav1.Condition{{Type: volumeapi.PoolConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}},
	}

	for _, test := range []struct {
		name, phase, marker string
		action              fsm.Action
		complete            bool
	}{
		{name: "copy repair", phase: string(fsm.PhaseCopying), marker: "path:.shiftpv/incoming/" + incoming.CopyID, action: fsm.ActionEnsureCopy},
		{name: "promotion repair", phase: string(fsm.PhasePromoting), marker: "path:volumes/" + volumeID, action: fsm.ActionEnsurePromotion},
		{name: "completed copy waits for ordinary inventory", phase: string(fsm.PhaseCopying), marker: "path:.shiftpv/incoming/" + incoming.CopyID, action: fsm.ActionWait, complete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			move := baseMove
			move.Status.Phase = test.phase
			destination := volumeapi.Pool{
				Name: destinationCopy.PoolName, UID: destinationCopy.PoolUID, NodeName: destinationCopy.NodeName,
				MountPath: "/destination-pool", Generation: 1, Status: readyStatus,
			}
			destination.Status.Inventory = &volumeapi.PoolInventory{
				ObservedAt: metav1.NewTime(now), Message: "CopyObservationProblem",
				Copies: []volumeapi.CopyObservation{{Marker: test.marker, Present: true, Problem: "UnrecordedPath"}},
			}
			repository := &memoryRepository{
				volumes: map[string]volumeapi.State{volumeID: {
					UID: sourceCopy.VolumeUID, Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: move.Name, CurrentCopy: &sourceCopy,
				}},
				pools: []volumeapi.Pool{source, destination}, readyPools: []volumeapi.Pool{source}, readyPoolsConfigured: true, moves: []volumeapi.Move{move},
			}
			objects := mobilityObjects(volumeID)
			replacement := objects[len(objects)-1].(*corev1.Pod)
			replacement.Spec.NodeName = "destination"
			replacement.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: placementHoldName}}
			reconciler := &Reconciler{Client: fake.NewSimpleClientset(objects...), Repository: repository, Namespace: "system", HelperImage: "helper", Now: func() time.Time { return now }}
			placement := reconciler.placementPod(move, replacement, namesFor(move.Name))
			placement.UID = "placement-uid"
			placement.Spec.NodeName = "destination"
			if _, err := reconciler.Client.CoreV1().Pods("system").Create(context.Background(), placement, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if test.complete {
				job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: namesFor(move.Name).CopyJob, Namespace: "system"}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
				if _, err := reconciler.Client.BatchV1().Jobs("system").Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			observed, err := reconciler.observe(context.Background(), move)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := fsm.Decide(fsm.Phase(test.phase), observed.FSM)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != test.action || observed.FSM.DestinationUnavailable != test.complete {
				t.Fatalf("observation=%#v decision=%#v", observed.FSM, decision)
			}
		})
	}
}

func TestObserveRejectsForeignServingCopyButAllowsCurrentDestinationCopy(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	sourceCopy, _, destinationCopy := testCopyIdentities(volumeID, "source", "destination")
	move := volumeapi.Move{
		Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{
			Phase: string(fsm.PhaseCopying), ConsumerName: "consumer", CandidateNodes: []string{"destination"},
			DestinationNode: "destination", DestinationPoolUID: destinationCopy.PoolUID, DestinationCopy: &destinationCopy,
		},
	}
	source := volumeapi.Pool{
		Name: sourceCopy.PoolName, UID: sourceCopy.PoolUID, NodeName: sourceCopy.NodeName, MountPath: "/source-pool",
		Status: volumeapi.PoolStatus{Inventory: &volumeapi.PoolInventory{Valid: true, Copies: []volumeapi.CopyObservation{{Identity: &sourceCopy, Present: true}}}},
	}
	destination := volumeapi.Pool{
		Name: destinationCopy.PoolName, UID: destinationCopy.PoolUID, NodeName: destinationCopy.NodeName, MountPath: "/destination-pool",
		Status: volumeapi.PoolStatus{Inventory: &volumeapi.PoolInventory{Valid: true, Copies: []volumeapi.CopyObservation{{Identity: &destinationCopy, Present: true}}}},
	}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {
			UID: sourceCopy.VolumeUID, Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: move.Name, CurrentCopy: &sourceCopy,
		}},
		pools: []volumeapi.Pool{source, destination}, readyPools: []volumeapi.Pool{source, destination}, readyPoolsConfigured: true,
		moves: []volumeapi.Move{move},
	}
	reconciler := &Reconciler{Client: fake.NewSimpleClientset(mobilityObjects(volumeID)...), Repository: repository, Namespace: "system", HelperImage: "helper"}

	observed, err := reconciler.observe(context.Background(), move)
	if err != nil {
		t.Fatal(err)
	}
	if observed.FSM.DestinationUnavailable || observed.FSM.UnsafeReason == "DestinationServingCopyPresent" {
		t.Fatalf("current transaction destination copy was rejected: %#v", observed.FSM)
	}

	foreign := destinationCopy
	foreign.CopyID = "foreign-copy"
	destination.Status.Inventory.Copies = append(destination.Status.Inventory.Copies, volumeapi.CopyObservation{Identity: &foreign, Present: true})
	repository.readyPools = []volumeapi.Pool{source, destination}
	observed, err = reconciler.observe(context.Background(), move)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.FSM.DestinationUnavailable || observed.FSM.UnsafeReason != "DestinationServingCopyPresent" {
		t.Fatalf("foreign serving copy was admitted: %#v", observed.FSM)
	}
}

func TestObserveBlocksApprovedDestinationAfterPoolRecreation(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{
		Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{
			Phase: string(fsm.PhaseCopying), ConsumerName: "consumer",
			CandidateNodes: []string{"destination"}, DestinationNode: "destination",
			DestinationPoolUID: "admitted-pool-uid", CapacityApproved: true,
		},
	}
	source := volumeapi.Pool{Name: "source", UID: "source-pool-uid", NodeName: "source", MountPath: "/source-pool"}
	recreated := volumeapi.Pool{Name: "destination", UID: "replacement-pool-uid", NodeName: "destination", MountPath: "/destination-pool"}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: move.Name}},
		pools:   []volumeapi.Pool{source, recreated},
		moves:   []volumeapi.Move{move},
	}
	reconciler := &Reconciler{Client: fake.NewSimpleClientset(mobilityObjects(volumeID)...), Repository: repository, Namespace: "system", HelperImage: "helper"}
	observed, err := reconciler.observe(context.Background(), move)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.FSM.DestinationBlocked || observed.FSM.UnsafeReason != "DestinationPoolIdentityChanged" {
		t.Fatalf("destination observation = %#v", observed.FSM)
	}
	decision, err := fsm.Decide(fsm.PhaseCopying, observed.FSM)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != fsm.ActionMarkBlocked || decision.Reason != "DestinationPoolIdentityChanged" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestObserveKeepsTerminatingPlacementReservedUntilNotFound(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	_, _, destination := testCopyIdentities(volumeID, "source", "destination")
	move := volumeapi.Move{
		Name: "move-test", UID: "move-uid", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{
			Phase: string(fsm.PhaseReleasingDestination), ConsumerName: "old-consumer", ConsumerUID: "old-consumer-uid",
			ReplacementName: "consumer", ReplacementUID: "consumer-uid", CandidateNodes: []string{"destination"}, DestinationNode: "destination", DestinationPoolUID: destination.PoolUID, DestinationCopy: &destination,
		},
	}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {UID: destination.VolumeUID, Phase: volumeapi.PhaseReady, OwnerNode: "destination", ActiveMove: move.Name, CurrentCopy: &destination}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/source-pool"}, {Name: "destination", NodeName: "destination", MountPath: "/destination-pool"}},
		moves:   []volumeapi.Move{move},
	}
	objects := mobilityObjects(volumeID)
	replacement := objects[len(objects)-1].(*corev1.Pod)
	replacement.Spec.NodeName = ""
	replacement.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: placementHoldName}}
	reconciler := &Reconciler{Client: fake.NewSimpleClientset(objects...), Repository: repository, Namespace: "system", HelperImage: "helper"}
	placement := reconciler.placementPod(move, replacement, namesFor(move.Name))
	placement.UID = "placement-uid"
	placement.Spec.NodeName = "destination"
	now := metav1.Now()
	placement.DeletionTimestamp = &now
	placement.Finalizers = []string{"test.shiftpv.io/hold"}
	if _, err := reconciler.Client.CoreV1().Pods("system").Create(context.Background(), placement, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	observed, err := reconciler.observe(context.Background(), move)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.FSM.PlacementExists || observed.FSM.DestinationScheduled {
		t.Fatalf("terminating placement observation = %#v", observed.FSM)
	}
	decision, err := fsm.Decide(fsm.PhaseReleasingDestination, observed.FSM)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != fsm.ActionDeletePlacement || decision.Next != fsm.PhaseReleasingDestination {
		t.Fatalf("terminating placement released workload: %#v", decision)
	}
}

func TestObserveAndExecuteMobilityActions(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{Name: "move-test", UID: "move-uid", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"}, Status: volumeapi.MoveStatus{Phase: string(fsm.PhasePending)}}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "source", PublishedNodes: []string{"source"}}},
		pools:   []volumeapi.Pool{{Name: "source", UID: "source-pool-uid", NodeName: "source", MountPath: "/source-pool"}, {Name: "destination", UID: "destination-pool-uid", NodeName: "destination", MountPath: "/destination-pool"}},
		moves:   []volumeapi.Move{move},
	}
	client := fake.NewSimpleClientset(mobilityObjects(volumeID)...)
	assignJobUIDs(client)
	reconciler := &Reconciler{
		Client: client, Repository: repository, Namespace: "system", HelperImage: "helper", ServiceAccountName: "shiftpv-controller",
		Cleanups: newTestCleanupStore(), CleanupOperator: receiptCleanupOperator{},
	}

	observed, err := reconciler.observe(ctx, move)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.FSM.PreconditionsValid || len(observed.CandidateNodes) != 1 {
		t.Fatalf("observation = %#v", observed)
	}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionLockVolume}); err != nil {
		t.Fatal(err)
	}
	if repository.volumes[volumeID].Phase != volumeapi.PhaseMoving || move.Status.ConsumerName != "consumer" {
		t.Fatalf("locked state=%#v move=%#v", repository.volumes[volumeID], move)
	}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionEvictConsumer}); err != nil {
		t.Fatal(err)
	}
	if !move.Status.EvictionRequested {
		t.Fatal("eviction was not recorded")
	}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionWait}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.Action("Unknown")}); err == nil {
		t.Fatal("unknown action was accepted")
	}

	replacement := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: "workload", UID: "replacement-uid"},
		Spec:       corev1.PodSpec{SchedulingGates: []corev1.PodSchedulingGate{{Name: placementHoldName}}, Volumes: claimVolumes()},
	}
	if _, err := client.CoreV1().Pods("workload").Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	observed.Replacement = replacement
	observed.Names = namesFor(move.Name)
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionEnsurePlacement}); err != nil {
		t.Fatal(err)
	}
	placement, err := client.CoreV1().Pods("system").Get(ctx, observed.Names.PlacementPod, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	placement.Spec.NodeName = "destination"
	if _, err := client.CoreV1().Pods("system").Update(ctx, placement, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	observed.Placement = placement
	observed.DestinationNode = "destination"
	move.Status.CapacityApproved = true
	move.Status.DestinationPoolUID = "destination-pool-uid"
	move.Status.SourceBytes = 1
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionEnsureCopy}); err != nil {
		t.Fatal(err)
	}
	updated, _ := client.CoreV1().Pods("workload").Get(ctx, "replacement", metav1.GetOptions{})
	if !hasPlacementHold(updated) || updated.Spec.NodeSelector[corev1.LabelHostname] != "destination" {
		t.Fatalf("replacement was not held and pinned: %#v", updated.Spec)
	}
	if _, err := client.BatchV1().Jobs("system").Get(ctx, observed.Names.CopyJob, metav1.GetOptions{}); err == nil {
		t.Fatal("copy Job was created before source readiness")
	}
	source, err := client.CoreV1().Pods("system").Get(ctx, observed.Names.SourcePod, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := source.Spec.Volumes[0].HostPath.Path; got != "/source-pool" {
		t.Fatalf("source HostPath = %q", got)
	}
	source.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := client.CoreV1().Pods("system").UpdateStatus(ctx, source, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionEnsureCopy}); err != nil {
		t.Fatal(err)
	}
	copyJob, err := client.BatchV1().Jobs("system").Get(ctx, observed.Names.CopyJob, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := copyJob.Spec.Template.Spec.Volumes[0].HostPath.Path; got != "/destination-pool" {
		t.Fatalf("destination HostPath = %q", got)
	}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionEnsurePromotion}); err != nil {
		t.Fatal(err)
	}
	unpublished := repository.volumes[volumeID]
	unpublished.PublishedNodes = nil // kubelet has completed the source unpublish.
	repository.volumes[volumeID] = unpublished
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionCommitOwner}); err != nil {
		t.Fatal(err)
	}
	if repository.volumes[volumeID].OwnerNode != "destination" {
		t.Fatalf("owner was not committed: %#v", repository.volumes[volumeID])
	}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionDeletePlacement}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionReleasePlacement}); err != nil {
		t.Fatal(err)
	}
	updated, _ = client.CoreV1().Pods("workload").Get(ctx, "replacement", metav1.GetOptions{})
	if hasPlacementHold(updated) || updated.Annotations[placementAnnotationKey] != "owner" {
		t.Fatalf("placement hold was not released as owner after commit: %#v", updated)
	}
	published := repository.volumes[volumeID]
	published.PublishedNodes = []string{"destination"}
	repository.volumes[volumeID] = published
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionEnsureCleanup}); err != nil {
		t.Fatal(err)
	}
	observed.Volume = repository.volumes[volumeID]
	move.Status.Phase = string(fsm.PhaseCompleting)
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionMarkSucceeded}); err != nil {
		t.Fatalf("%v: move=%#v volume=%#v", err, move.Status, observed.Volume)
	}
	if repository.volumes[volumeID].ActiveMove != "" {
		t.Fatalf("active move was not cleared: %#v", repository.volumes[volumeID])
	}
	if _, err := client.CoreV1().Secrets("system").Get(ctx, observed.Names.Secret, metav1.GetOptions{}); err == nil {
		t.Fatal("transfer Secret was not deleted")
	}
}

func TestJobStateAndBlockedVolume(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"}, Status: volumeapi.MoveStatus{Phase: string(fsm.PhasePending)}}
	repository := &memoryRepository{volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseMoving, OwnerNode: "source", ActiveMove: move.Name}}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "complete", Namespace: "system"}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
	reconciler := &Reconciler{Client: fake.NewSimpleClientset(job), Repository: repository, Namespace: "system", HelperImage: "helper"}
	complete, failed, err := reconciler.jobState(ctx, "complete")
	if err != nil || !complete || failed {
		t.Fatalf("job state complete=%v failed=%v err=%v", complete, failed, err)
	}
	observed := observation{Volume: identifiedTestState(volumeID, repository.volumes[volumeID], repository.pools)}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionMarkBlocked, Reason: "CopyFailed"}); err != nil {
		t.Fatal(err)
	}
	if repository.volumes[volumeID].Phase != volumeapi.PhaseBlocked {
		t.Fatalf("volume was not blocked: %#v", repository.volumes[volumeID])
	}
}

func TestBlockedBeforeLockClosesRediscovery(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"}, Status: volumeapi.MoveStatus{Phase: string(fsm.PhasePending)}}
	repository := &memoryRepository{volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "source"}}, moves: []volumeapi.Move{move}}
	reconciler := &Reconciler{Client: fake.NewSimpleClientset(readyNode("source", true)), Repository: repository, Namespace: "system", HelperImage: "helper"}
	observed := observation{Volume: identifiedTestState(volumeID, repository.volumes[volumeID], repository.pools)}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionMarkBlocked, Reason: "ControlledConsumerMissing"}); err != nil {
		t.Fatal(err)
	}
	state := repository.volumes[volumeID]
	if state.Phase != volumeapi.PhaseReady || state.ActiveMove != "" || state.OwnerNode != "source" {
		t.Fatalf("pre-lock failure changed volume state: %#v", state)
	}
	repository.moves[0].Status.Phase = string(fsm.PhaseBlocked)
	observed.Volume = volumeapi.State{Phase: volumeapi.PhaseReady, OwnerNode: "source", ActiveMove: "another-move"}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionMarkBlocked, Reason: "Conflict"}); err == nil {
		t.Fatal("conflicting active move was overwritten")
	}
	if err := reconciler.discoverMoves(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repository.moves) != 1 {
		t.Fatalf("blocked volume was rediscovered: %#v", repository.moves)
	}
}

func TestReconcileAllAndCanceledRun(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{Name: "move-test", UID: "move-uid", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"}, Status: volumeapi.MoveStatus{Phase: string(fsm.PhasePending)}}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "source", PublishedNodes: []string{"source"}}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/pool"}, {Name: "destination", NodeName: "destination", MountPath: "/pool"}},
		moves:   []volumeapi.Move{move},
	}
	reconciler := &Reconciler{Client: fake.NewSimpleClientset(mobilityObjects(volumeID)...), Repository: repository, Namespace: "system", HelperImage: "helper", Interval: 1}
	if err := reconciler.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.moves[0].Status.Phase != string(fsm.PhaseLocking) || repository.volumes[volumeID].Phase != volumeapi.PhaseMoving {
		t.Fatalf("move=%#v volume=%#v", repository.moves[0], repository.volumes[volumeID])
	}
	repository.moves[0].Status.Phase = string(fsm.PhaseSucceeded)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reconciler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := (&Reconciler{}).validate(); err == nil {
		t.Fatal("invalid reconciler was accepted")
	}
}

func TestRunReconcilesImmediatelyOnWake(t *testing.T) {
	repository := &countingRepository{memoryRepository: memoryRepository{volumes: map[string]volumeapi.State{}}}
	wake := make(chan struct{}, 1)
	reconciler := &Reconciler{
		Client: fake.NewSimpleClientset(), Repository: repository,
		Namespace: "system", HelperImage: "helper", Interval: time.Hour, Wake: wake,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	waitForCalls := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for repository.listVolumeCalls.Load() < want && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := repository.listVolumeCalls.Load(); got < want {
			t.Fatalf("ListVolumes calls = %d, want at least %d", got, want)
		}
	}
	waitForCalls(1)
	time.Sleep(20 * time.Millisecond)
	if got := repository.listVolumeCalls.Load(); got != 1 {
		t.Fatalf("idle reconciler performed %d ListVolumes calls before an event", got)
	}
	wake <- struct{}{}
	waitForCalls(2)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconciler did not stop after cancellation")
	}
}

func mobilityObjects(volumeID string) []runtime.Object {
	controller := true
	return []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload", Labels: map[string]string{admissionNamespaceLabel: "enabled"}}},
		readyNode("source", true), readyNode("destination", false),
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv"}, Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.shiftpv.io", VolumeHandle: volumeID}},
			ClaimRef:               &corev1.ObjectReference{Name: "claim", Namespace: "workload", UID: "claim-uid"},
		}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "workload", UID: "claim-uid"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "workload", UID: "rs"}, Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: claimVolumes()}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "workload", UID: "consumer-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: types.UID("rs"), Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: "source", Volumes: claimVolumes()}},
	}
}

func readyNode(name string, cordoned bool) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name}}, Spec: corev1.NodeSpec{Unschedulable: cordoned}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}

func claimVolumes() []corev1.Volume {
	return []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "claim"}}}}
}
