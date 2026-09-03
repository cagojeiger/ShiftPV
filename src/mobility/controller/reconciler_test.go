package controller

import (
	"context"
	"fmt"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
)

type memoryRepository struct {
	volumes map[string]volumeapi.State
	pools   []volumeapi.Pool
	moves   []volumeapi.Move
}

func (m *memoryRepository) ListVolumes(context.Context) (map[string]volumeapi.State, error) {
	return m.volumes, nil
}
func (m *memoryRepository) Get(_ context.Context, id string) (volumeapi.State, error) {
	state, exists := m.volumes[id]
	if !exists {
		return volumeapi.State{}, fmt.Errorf("volume not found")
	}
	return state, nil
}
func (m *memoryRepository) CompareAndSetState(_ context.Context, id, phase, active, owner string, next volumeapi.State) error {
	current := m.volumes[id]
	if current.Phase != phase || current.ActiveMove != active || current.OwnerNode != owner {
		return volumeapi.ErrStateConflict
	}
	m.volumes[id] = next
	return nil
}
func (m *memoryRepository) Pools(context.Context) ([]volumeapi.Pool, error) { return m.pools, nil }
func (m *memoryRepository) CreateMove(_ context.Context, _ string, spec volumeapi.MoveSpec) (volumeapi.Move, error) {
	move := volumeapi.Move{Name: "move-generated", UID: "uid", Spec: spec}
	m.moves = append(m.moves, move)
	return move, nil
}
func (m *memoryRepository) ListMoves(context.Context) ([]volumeapi.Move, error) { return m.moves, nil }
func (m *memoryRepository) SetMoveStatus(_ context.Context, name string, status volumeapi.MoveStatus) error {
	for index := range m.moves {
		if m.moves[index].Name == name {
			m.moves[index].Status = status
			return nil
		}
	}
	return fmt.Errorf("move not found")
}

func TestDiscoverMovesCreatesOneMoveForHealthyCordon(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "source", PublishedNodes: []string{"source"}}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/pool"}, {Name: "destination", NodeName: "destination", MountPath: "/pool"}},
	}
	client := fake.NewSimpleClientset(mobilityObjects(volumeID)...)
	reconciler := &Reconciler{Client: client, Repository: repository, Namespace: "system", HelperImage: "helper"}
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

func TestObserveRecognizesOwnerCommitBeforeMovePhasePersistence(t *testing.T) {
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{
		Name: "move-test", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"},
		Status: volumeapi.MoveStatus{Phase: string(fsm.PhaseCommitting), ConsumerName: "consumer", DestinationNode: "destination"},
	}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "destination", ActiveMove: move.Name}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/source-pool"}, {Name: "destination", NodeName: "destination", MountPath: "/destination-pool"}},
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
}

func TestObserveAndExecuteMobilityActions(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	move := volumeapi.Move{Name: "move-test", UID: "move-uid", Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: "source"}, Status: volumeapi.MoveStatus{Phase: string(fsm.PhasePending)}}
	repository := &memoryRepository{
		volumes: map[string]volumeapi.State{volumeID: {Phase: volumeapi.PhaseReady, OwnerNode: "source", PublishedNodes: []string{"source"}}},
		pools:   []volumeapi.Pool{{Name: "source", NodeName: "source", MountPath: "/source-pool"}, {Name: "destination", NodeName: "destination", MountPath: "/destination-pool"}},
		moves:   []volumeapi.Move{move},
	}
	client := fake.NewSimpleClientset(mobilityObjects(volumeID)...)
	reconciler := &Reconciler{Client: client, Repository: repository, Namespace: "system", HelperImage: "helper"}

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
		ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: "workload"},
		Spec:       corev1.PodSpec{NodeName: "destination", SchedulingGates: []corev1.PodSchedulingGate{{Name: placementHoldName}}, Volumes: claimVolumes()},
	}
	if _, err := client.CoreV1().Pods("workload").Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	observed.Replacement = replacement
	observed.DestinationNode = "destination"
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionReleasePlacement}); err != nil {
		t.Fatal(err)
	}
	updated, _ := client.CoreV1().Pods("workload").Get(ctx, "replacement", metav1.GetOptions{})
	if hasPlacementHold(updated) {
		t.Fatal("placement hold was not released")
	}

	observed.Replacement = updated
	observed.Names = namesFor(move.Name)
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionEnsureCopy}); err != nil {
		t.Fatal(err)
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
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionCommitOwner}); err != nil {
		t.Fatal(err)
	}
	if repository.volumes[volumeID].OwnerNode != "destination" {
		t.Fatalf("owner was not committed: %#v", repository.volumes[volumeID])
	}
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionEnsureCleanup}); err != nil {
		t.Fatal(err)
	}
	observed.Volume = repository.volumes[volumeID]
	if err := reconciler.execute(ctx, &move, observed, fsm.Decision{Action: fsm.ActionMarkSucceeded}); err != nil {
		t.Fatal(err)
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
	observed := observation{Volume: repository.volumes[volumeID]}
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
	observed := observation{Volume: repository.volumes[volumeID]}
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

func mobilityObjects(volumeID string) []runtime.Object {
	controller := true
	return []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload", Labels: map[string]string{admissionNamespaceLabel: "enabled"}}},
		readyNode("source", true), readyNode("destination", false),
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv"}, Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.shiftpv.io", VolumeHandle: volumeID}},
			ClaimRef:               &corev1.ObjectReference{Name: "claim", Namespace: "workload"},
		}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "workload"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "workload", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: types.UID("rs"), Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: "source", Volumes: claimVolumes()}},
	}
}

func readyNode(name string, cordoned bool) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{Unschedulable: cordoned}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}

func claimVolumes() []corev1.Volume {
	return []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "claim"}}}}
}
