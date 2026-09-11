package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

type fakeVolumeRegistry struct {
	state     volumeapi.State
	poolNodes []string
}

type retryDeleteVolumeRegistry struct {
	state       volumeapi.State
	exists      bool
	deleteCalls int
	deleteFirst bool
}

func (r *retryDeleteVolumeRegistry) BeginCreate(context.Context, string, string) (volumeapi.State, error) {
	return r.state, nil
}

func (r *retryDeleteVolumeRegistry) CompleteCreate(context.Context, string, string, volume.CopyIdentity) error {
	return nil
}

func (r *retryDeleteVolumeRegistry) Get(_ context.Context, id string) (volumeapi.State, error) {
	if !r.exists {
		return volumeapi.State{}, apierrors.NewNotFound(schema.GroupResource{Group: "shiftpv.io", Resource: "shiftpvvolumes"}, id)
	}
	return r.state, nil
}

func (r *retryDeleteVolumeRegistry) Delete(_ context.Context, _ string, uid string) error {
	if r.exists && r.state.UID != uid {
		return volumeapi.ErrStateConflict
	}
	r.deleteCalls++
	if r.deleteCalls == 1 {
		if r.deleteFirst {
			r.exists = false
		}
		return apierrors.NewTimeoutError("volume state delete response timed out", 1)
	}
	r.exists = false
	return nil
}

func (r *retryDeleteVolumeRegistry) BeginDelete(_ context.Context, volumeID, uid string, copy volume.CopyIdentity) (volumeapi.State, error) {
	if !r.exists || r.state.UID != uid || copy.VolumeID != volumeID || r.state.CurrentCopy == nil || *r.state.CurrentCopy != copy ||
		r.state.ActiveMove != "" || len(r.state.PublishedNodes) != 0 ||
		(r.state.Phase != volumeapi.PhaseReady && r.state.Phase != volumeapi.PhaseDeleting) {
		return volumeapi.State{}, volumeapi.ErrStateConflict
	}
	r.state.Phase = volumeapi.PhaseDeleting
	r.state.DeletionOperationID = "delete-" + uid
	return r.state, nil
}

func (*retryDeleteVolumeRegistry) PoolNodes(context.Context) ([]string, error) { return nil, nil }

func (f *fakeVolumeRegistry) Get(context.Context, string) (volumeapi.State, error) {
	return f.state, nil
}
func (f *fakeVolumeRegistry) Delete(_ context.Context, _ string, uid string) error {
	if f.state.UID != "" && f.state.UID != uid {
		return volumeapi.ErrStateConflict
	}
	return nil
}
func (f *fakeVolumeRegistry) PoolNodes(context.Context) ([]string, error) {
	if len(f.poolNodes) > 0 {
		return f.poolNodes, nil
	}
	return []string{f.state.OwnerNode}, nil
}

func (f *fakeVolumeRegistry) BeginCreate(_ context.Context, volumeID, nodeName string) (volumeapi.State, error) {
	if f.state.CurrentCopy == nil {
		copy := volume.CopyIdentity{
			InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid",
			VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "initial-volume-uid",
			NodeName: nodeName, Role: volume.RoleServing,
		}
		f.state = volumeapi.State{UID: copy.VolumeUID, Phase: volumeapi.PhasePending, OwnerNode: nodeName, CurrentCopy: &copy}
	}
	return f.state, nil
}

func (f *fakeVolumeRegistry) CompleteCreate(_ context.Context, _ string, _ string, copy volume.CopyIdentity) error {
	f.state.Phase = volumeapi.PhaseReady
	f.state.CurrentCopy = &copy
	return nil
}

func (f *fakeVolumeRegistry) BeginDelete(_ context.Context, volumeID, uid string, copy volume.CopyIdentity) (volumeapi.State, error) {
	if f.state.UID != uid || copy.VolumeID != volumeID || f.state.CurrentCopy == nil || *f.state.CurrentCopy != copy ||
		f.state.ActiveMove != "" || len(f.state.PublishedNodes) != 0 ||
		(f.state.Phase != volumeapi.PhaseReady && f.state.Phase != volumeapi.PhaseDeleting) {
		return volumeapi.State{}, volumeapi.ErrStateConflict
	}
	f.state.Phase = volumeapi.PhaseDeleting
	f.state.DeletionOperationID = "delete-" + uid
	return f.state, nil
}

type fakeDirectoryOperator struct {
	createdNode string
	createdID   string
	createCalls int
	createErr   error
}

type retryableDirectoryError struct{ error }

func (retryableDirectoryError) Retryable() bool { return true }

type durableCreateRegistry struct {
	fakeVolumeRegistry
	state         volumeapi.State
	events        *[]string
	beginErr      error
	completeErr   error
	completeCalls int
}

func (r *durableCreateRegistry) BeginCreate(context.Context, string, string) (volumeapi.State, error) {
	*r.events = append(*r.events, "intent")
	return r.state, r.beginErr
}

func (r *durableCreateRegistry) CompleteCreate(context.Context, string, string, volume.CopyIdentity) error {
	*r.events = append(*r.events, "complete")
	r.completeCalls++
	return r.completeErr
}

type identityCreateOperator struct {
	fakeDirectoryOperator
	events      *[]string
	createErr   error
	finalizeErr error
}

type receiptCleanupOperator struct{ calls int }

type blockingCreateOperator struct {
	started chan struct{}
	release chan struct{}
}

func (o *blockingCreateOperator) CreateCopy(context.Context, volume.CopyIdentity) error {
	o.started <- struct{}{}
	<-o.release
	return nil
}

func (*blockingCreateOperator) FinalizeCreate(context.Context, volume.CopyIdentity) error { return nil }

type blockingCleanupOperator struct {
	started  chan struct{}
	release  chan struct{}
	calls    atomic.Int32
	delegate receiptCleanupOperator
}

func (o *blockingCleanupOperator) Reclaim(ctx context.Context, cleanup cleanupapi.Cleanup, store *cleanupapi.Store) (cleanupapi.Cleanup, error) {
	if o.calls.Add(1) == 1 {
		o.started <- struct{}{}
		<-o.release
	}
	return o.delegate.Reclaim(ctx, cleanup, store)
}

func configuredService(service *Service) *Service {
	if service.Volumes == nil {
		service.Volumes = &fakeVolumeRegistry{poolNodes: []string{"worker-a", "worker-b"}}
	}
	if service.Cleanups == nil {
		client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{cleanupapi.Resource: "ShiftPVCleanupList"})
		client.PrependReactor("create", "shiftpvcleanups", func(action k8stesting.Action) (bool, runtime.Object, error) {
			object := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
			object.SetUID("cleanup-uid")
			return false, nil, nil
		})
		service.Cleanups = &cleanupapi.Store{Client: client}
	}
	if service.CleanupOperator == nil {
		service.CleanupOperator = &receiptCleanupOperator{}
	}
	return service
}

func (o *receiptCleanupOperator) Reclaim(ctx context.Context, cleanup cleanupapi.Cleanup, store *cleanupapi.Store) (cleanupapi.Cleanup, error) {
	o.calls++
	if cleanup.Status.Phase == cleanupapi.PhaseVerifying || cleanup.Status.Phase == cleanupapi.PhaseCompleted {
		return cleanup, nil
	}
	executor := &cleanupapi.Executor{JobName: "job", JobUID: "job-uid", NodeName: cleanup.Spec.Target.NodeName}
	if err := store.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
		return cleanupapi.Cleanup{}, err
	}
	receipt := &cleanupapi.Receipt{
		OperationID: cleanup.Spec.OperationID, ExecutorUID: executor.JobUID,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true,
	}
	if err := store.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: executor, Receipt: receipt}); err != nil {
		return cleanupapi.Cleanup{}, err
	}
	return store.Get(ctx, cleanup.Name)
}

func TestDeleteVolumeUsesCleanupIntentAndReceiptBeforeMetadata(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-0123456789abcdef0123456789abcdef"
	copy := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid",
		VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "copy-id",
		NodeName: "worker-a", Role: volume.RoleServing,
	}
	registry := &retryDeleteVolumeRegistry{state: volumeapi.State{UID: copy.VolumeUID, Phase: volumeapi.PhaseReady, OwnerNode: copy.NodeName, CurrentCopy: &copy}, exists: true, deleteCalls: 1}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{cleanupapi.Resource: "ShiftPVCleanupList"})
	dynamicClient.PrependReactor("create", "shiftpvcleanups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID("cleanup-uid")
		return false, nil, nil
	})
	cleanups := &cleanupapi.Store{Client: dynamicClient}
	operator := &fakeDirectoryOperator{}
	reclaimer := &receiptCleanupOperator{}
	client := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: volumeID, Namespace: "shiftpv-system"}, Data: map[string]string{"nodeName": "worker-a"}})
	service := &Service{
		Client: client, Namespace: "shiftpv-system", Operator: operator, Volumes: registry,
		Cleanups: cleanups, CleanupOperator: reclaimer,
	}
	if _, err := service.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatal(err)
	}
	if reclaimer.calls != 1 {
		t.Fatalf("cleanup calls=%d", reclaimer.calls)
	}
	if registry.state.Phase != volumeapi.PhaseDeleting || registry.state.DeletionOperationID != "delete-"+copy.VolumeUID {
		t.Fatalf("volume deletion was not fenced before cleanup: %#v", registry.state)
	}
	items, err := cleanups.List(ctx)
	if err != nil || len(items) != 1 || items[0].Status.Phase != cleanupapi.PhaseCompleted || items[0].Status.Receipt == nil {
		t.Fatalf("cleanups=%#v err=%v", items, err)
	}
	if _, err := client.CoreV1().ConfigMaps("shiftpv-system").Get(ctx, volumeID, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("reservation remains: %v", err)
	}
	if registry.exists {
		t.Fatal("volume metadata remains after verified receipt")
	}
}

func TestCreateVolumeIsBlockedByUnresolvedCleanupFence(t *testing.T) {
	request := validCreateRequest("worker-a")
	volumeID, err := volume.IDFromName(request.Name)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{cleanupapi.Resource: "ShiftPVCleanupList"})
	cleanups := &cleanupapi.Store{Client: dynamicClient}
	target := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid", VolumeID: volumeID,
		VolumeUID: "old-volume-uid", CopyID: "old-copy", NodeName: "worker-a", Role: volume.RoleServing,
	}
	if _, err := cleanups.Ensure(context.Background(), cleanupapi.Spec{
		OperationID: "review-old-copy", Target: target, Reason: "OrphanReclaim",
		Authority: cleanupapi.Authority{Kind: "Namespace", Name: "kube-system", UID: target.InstallationID},
	}); err != nil {
		t.Fatal(err)
	}
	service := configuredService(&Service{
		Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}, Cleanups: cleanups,
	})
	if _, err := service.CreateVolume(context.Background(), request); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unresolved cleanup did not fence volume recreation: %v", err)
	}
}

func TestDeleteVolumeConvergesAfterAcceptedReservationDeleteTimeout(t *testing.T) {
	ctx := context.Background()
	volumeID := "shiftpv-1123456789abcdef0123456789abcdef"
	copy := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid",
		VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "copy-id",
		NodeName: "worker-a", Role: volume.RoleServing,
	}
	registry := &retryDeleteVolumeRegistry{
		state:  volumeapi.State{UID: copy.VolumeUID, Phase: volumeapi.PhaseReady, OwnerNode: copy.NodeName, CurrentCopy: &copy},
		exists: true, deleteCalls: 1,
	}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{cleanupapi.Resource: "ShiftPVCleanupList"})
	dynamicClient.PrependReactor("create", "shiftpvcleanups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID("cleanup-uid")
		return false, nil, nil
	})
	cleanups := &cleanupapi.Store{Client: dynamicClient}
	reclaimer := &receiptCleanupOperator{}
	reservation := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: volumeID, Namespace: "shiftpv-system", UID: "reservation-uid"},
		Data:       map[string]string{"nodeName": copy.NodeName, "volumeID": volumeID, "volumeUID": copy.VolumeUID},
	}
	client := fake.NewClientset(reservation)
	timedOut := false
	client.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if timedOut {
			return false, nil, nil
		}
		timedOut = true
		deleteAction := action.(k8stesting.DeleteAction)
		if err := client.Tracker().Delete(action.GetResource(), action.GetNamespace(), deleteAction.GetName()); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewTimeoutError("reservation delete response lost", 1)
	})
	service := &Service{
		Client: client, Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}, Volumes: registry,
		Cleanups: cleanups, CleanupOperator: reclaimer,
	}
	request := &csi.DeleteVolumeRequest{VolumeId: volumeID}
	if _, err := service.DeleteVolume(ctx, request); status.Code(err) != codes.Unavailable {
		t.Fatalf("first delete code=%s err=%v", status.Code(err), err)
	}
	if _, err := client.CoreV1().ConfigMaps("shiftpv-system").Get(ctx, volumeID, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("accepted reservation deletion was not retained: %v", err)
	}
	if _, err := service.DeleteVolume(ctx, request); err != nil {
		t.Fatalf("retry did not converge: %v", err)
	}
	if registry.exists || reclaimer.calls != 2 {
		t.Fatalf("exists=%t cleanup calls=%d", registry.exists, reclaimer.calls)
	}
}

func TestDeleteVolumeIsIdempotentAfterMetadataIsGone(t *testing.T) {
	volumeID := "shiftpv-2123456789abcdef0123456789abcdef"
	registry := &retryDeleteVolumeRegistry{exists: false}
	reclaimer := &receiptCleanupOperator{}
	service := configuredService(&Service{
		Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{},
		Volumes: registry, CleanupOperator: reclaimer,
	})
	if _, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatal(err)
	}
	if reclaimer.calls != 0 || registry.deleteCalls != 0 {
		t.Fatalf("completed delete repeated effects: cleanup=%d volumeDelete=%d", reclaimer.calls, registry.deleteCalls)
	}
}

func TestDeleteVolumePreservesReservationWithoutExactCopyState(t *testing.T) {
	volumeID := "shiftpv-3123456789abcdef0123456789abcdef"
	registry := &retryDeleteVolumeRegistry{
		state: volumeapi.State{UID: "volume-uid", Phase: volumeapi.PhaseReady, OwnerNode: "worker-a"}, exists: true,
	}
	reservation := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: volumeID, Namespace: "shiftpv-system"}, Data: map[string]string{"nodeName": "worker-a"}}
	client := fake.NewClientset(reservation)
	service := configuredService(&Service{
		Client: client, Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}, Volumes: registry,
	})
	if _, err := service.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing exact copy state was accepted: %v", err)
	}
	if _, err := client.CoreV1().ConfigMaps("shiftpv-system").Get(context.Background(), volumeID, metav1.GetOptions{}); err != nil {
		t.Fatalf("reservation was removed: %v", err)
	}
}

func TestConcurrentCreateVolumeCallsSerializeExactCopyEffect(t *testing.T) {
	operator := &blockingCreateOperator{started: make(chan struct{}, 2), release: make(chan struct{})}
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator})
	request := validCreateRequest("worker-a")
	done := make(chan error, 2)
	go func() {
		_, err := service.CreateVolume(context.Background(), request)
		done <- err
	}()
	<-operator.started
	go func() {
		_, err := service.CreateVolume(context.Background(), request)
		done <- err
	}()
	select {
	case <-operator.started:
		close(operator.release)
		t.Fatal("second exact copy effect crossed the volume lifecycle lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(operator.release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentDeleteVolumeCallsRunOneExactCleanup(t *testing.T) {
	volumeID := "shiftpv-4123456789abcdef0123456789abcdef"
	copy := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid",
		VolumeID: volumeID, VolumeUID: "volume-uid", CopyID: "copy-id",
		NodeName: "worker-a", Role: volume.RoleServing,
	}
	registry := &retryDeleteVolumeRegistry{
		state:  volumeapi.State{UID: copy.VolumeUID, Phase: volumeapi.PhaseReady, OwnerNode: copy.NodeName, CurrentCopy: &copy},
		exists: true, deleteCalls: 1,
	}
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{cleanupapi.Resource: "ShiftPVCleanupList"})
	dynamicClient.PrependReactor("create", "shiftpvcleanups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID("cleanup-uid")
		return false, nil, nil
	})
	cleaner := &blockingCleanupOperator{started: make(chan struct{}, 1), release: make(chan struct{})}
	service := &Service{
		Client:    fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: volumeID, Namespace: "shiftpv-system"}, Data: map[string]string{"nodeName": copy.NodeName}}),
		Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}, Volumes: registry,
		Cleanups: &cleanupapi.Store{Client: dynamicClient}, CleanupOperator: cleaner,
	}
	request := &csi.DeleteVolumeRequest{VolumeId: volumeID}
	done := make(chan error, 2)
	go func() {
		_, err := service.DeleteVolume(context.Background(), request)
		done <- err
	}()
	<-cleaner.started
	go func() {
		_, err := service.DeleteVolume(context.Background(), request)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if cleaner.calls.Load() != 1 {
		close(cleaner.release)
		t.Fatalf("concurrent cleanup effects=%d", cleaner.calls.Load())
	}
	close(cleaner.release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if cleaner.calls.Load() != 1 || registry.exists {
		t.Fatalf("cleanup calls=%d volume exists=%t", cleaner.calls.Load(), registry.exists)
	}
}

func (o *identityCreateOperator) CreateCopy(context.Context, volume.CopyIdentity) error {
	*o.events = append(*o.events, "effect")
	return o.createErr
}

func (o *identityCreateOperator) FinalizeCreate(context.Context, volume.CopyIdentity) error {
	*o.events = append(*o.events, "settle-helper")
	return o.finalizeErr
}

func TestCreateVolumeOrdersDurableIntentEffectAndCompletion(t *testing.T) {
	events := []string{}
	copy := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid",
		VolumeID: "shiftpv-0123456789abcdef0123456789abcdef", VolumeUID: "volume-uid",
		CopyID: "copy-id", NodeName: "worker-a", Role: volume.RoleServing,
	}
	registry := &durableCreateRegistry{state: volumeapi.State{UID: copy.VolumeUID, Phase: volumeapi.PhasePending, OwnerNode: copy.NodeName, CurrentCopy: &copy}, events: &events}
	registry.poolNodes = []string{"worker-a"}
	operator := &identityCreateOperator{events: &events}
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator, Volumes: registry})
	if _, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "intent,effect,complete,settle-helper" {
		t.Fatalf("lifecycle order=%s", got)
	}
}

func TestCreateVolumeNeverRunsEffectWithoutDurableIntent(t *testing.T) {
	events := []string{}
	registry := &durableCreateRegistry{events: &events, beginErr: apierrors.NewTimeoutError("lost response", 1)}
	operator := &identityCreateOperator{events: &events}
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator, Volumes: registry})
	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("intent failure code=%s err=%v", status.Code(err), err)
	}
	if got := strings.Join(events, ","); got != "intent" {
		t.Fatalf("effect ran without intent: %s", got)
	}
}

func TestCreateVolumeSettlesHelperOnlyAfterReadyIsDurable(t *testing.T) {
	events := []string{}
	copy := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid",
		VolumeID: "shiftpv-0123456789abcdef0123456789abcdef", VolumeUID: "volume-uid",
		CopyID: "copy-id", NodeName: "worker-a", Role: volume.RoleServing,
	}
	registry := &durableCreateRegistry{
		state:  volumeapi.State{UID: copy.VolumeUID, Phase: volumeapi.PhasePending, OwnerNode: copy.NodeName, CurrentCopy: &copy},
		events: &events, completeErr: apierrors.NewTimeoutError("ready response lost", 1),
	}
	registry.poolNodes = []string{"worker-a"}
	operator := &identityCreateOperator{events: &events}
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator, Volumes: registry})
	if _, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a")); status.Code(err) != codes.Unavailable {
		t.Fatalf("ready failure code=%s err=%v", status.Code(err), err)
	}
	if got := strings.Join(events, ","); got != "intent,effect,complete" {
		t.Fatalf("helper was settled before Ready became durable: %s", got)
	}
}

func TestCreateVolumeRequiresHelperSettlementBeforeSuccess(t *testing.T) {
	events := []string{}
	copy := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool", PoolUID: "pool-uid",
		VolumeID: "shiftpv-0123456789abcdef0123456789abcdef", VolumeUID: "volume-uid",
		CopyID: "copy-id", NodeName: "worker-a", Role: volume.RoleServing,
	}
	registry := &durableCreateRegistry{
		state:  volumeapi.State{UID: copy.VolumeUID, Phase: volumeapi.PhasePending, OwnerNode: copy.NodeName, CurrentCopy: &copy},
		events: &events,
	}
	registry.poolNodes = []string{"worker-a"}
	operator := &identityCreateOperator{events: &events, finalizeErr: retryableDirectoryError{errors.New("helper still terminating")}}
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator, Volumes: registry})
	if _, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a")); status.Code(err) != codes.Unavailable {
		t.Fatalf("settlement failure code=%s err=%v", status.Code(err), err)
	}
	if got := strings.Join(events, ","); got != "intent,effect,complete,settle-helper" {
		t.Fatalf("unexpected lifecycle order: %s", got)
	}
}

func (f *fakeDirectoryOperator) CreateCopy(_ context.Context, identity volume.CopyIdentity) error {
	f.createCalls++
	f.createdNode = identity.NodeName
	f.createdID = identity.VolumeID
	return f.createErr
}

func (f *fakeDirectoryOperator) FinalizeCreate(context.Context, volume.CopyIdentity) error {
	return nil
}

func TestCreateVolumeIsIdempotent(t *testing.T) {
	operator := &fakeDirectoryOperator{}
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator})
	req := validCreateRequest("worker-a")

	first, err := service.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Volume.VolumeId != second.Volume.VolumeId {
		t.Fatalf("volume ID changed: %q != %q", first.Volume.VolumeId, second.Volume.VolumeId)
	}
	if operator.createdNode != "worker-a" || operator.createdID != first.Volume.VolumeId {
		t.Fatalf("unexpected directory operation: node=%q id=%q", operator.createdNode, operator.createdID)
	}
}

func TestCreateVolumeRejectsProvisioningDuringUninstallQuiesce(t *testing.T) {
	operator := &fakeDirectoryOperator{}
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator, ProvisioningGate: rejectingProvisioningGate{}})
	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("CreateVolume code = %s, want Unavailable: %v", status.Code(err), err)
	}
	if operator.createCalls != 0 {
		t.Fatal("quiesced CreateVolume reached the directory operator")
	}
}

func TestCreateVolumeTopologyFollowsMobilityOptIn(t *testing.T) {
	for name, test := range map[string]struct {
		namespace *corev1.Namespace
		wantNodes []string
	}{
		"opted in": {
			namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mobile", Labels: map[string]string{MobilityAdmissionLabel: mobilityEnabledValue}}},
			wantNodes: []string{"worker-a", "worker-b"},
		},
		"not opted in": {
			namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "local"}},
			wantNodes: []string{"worker-a"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			namespace := test.namespace
			wantNodes := test.wantNodes
			registry := &fakeVolumeRegistry{state: volumeapi.State{OwnerNode: "worker-a"}, poolNodes: []string{"worker-a", "worker-b"}}
			service := configuredService(&Service{Client: fake.NewClientset(namespace), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}, Volumes: registry})
			req := validCreateRequest("worker-a")
			req.Parameters[PVCNameKey] = "claim"
			req.Parameters[PVCNamespaceKey] = namespace.Name
			req.Parameters[PVNameKey] = "pv-test"
			response, err := service.CreateVolume(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if got := topologyNodes(response.Volume.AccessibleTopology); !equalStrings(got, wantNodes) {
				t.Fatalf("accessible topology = %v, want %v", got, wantNodes)
			}
		})
	}
}

func TestCreateVolumeWithoutProvisionerMetadataStaysOwnerLocal(t *testing.T) {
	registry := &fakeVolumeRegistry{state: volumeapi.State{OwnerNode: "worker-a"}, poolNodes: []string{"worker-a", "worker-b"}}
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}, Volumes: registry})
	response, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := topologyNodes(response.Volume.AccessibleTopology); !equalStrings(got, []string{"worker-a"}) {
		t.Fatalf("accessible topology = %v, want owner only", got)
	}
}

func TestCreateVolumeRejectsChangedSelectedNode(t *testing.T) {
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}})
	if _, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a")); err != nil {
		t.Fatal(err)
	}
	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-b"))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists, got %v", err)
	}
}

func TestCreateVolumeRejectsInvalidRequests(t *testing.T) {
	tests := map[string]func(*csi.CreateVolumeRequest){
		"missing name":     func(req *csi.CreateVolumeRequest) { req.Name = "" },
		"missing capacity": func(req *csi.CreateVolumeRequest) { req.CapacityRange = nil },
		"zero capacity": func(req *csi.CreateVolumeRequest) {
			req.CapacityRange.RequiredBytes = 0
		},
		"capacity above limit": func(req *csi.CreateVolumeRequest) {
			req.CapacityRange.LimitBytes = req.CapacityRange.RequiredBytes - 1
		},
		"missing capabilities": func(req *csi.CreateVolumeRequest) {
			req.VolumeCapabilities = nil
		},
		"raw block": func(req *csi.CreateVolumeRequest) {
			req.VolumeCapabilities[0].AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
		},
		"unsupported access mode": func(req *csi.CreateVolumeRequest) {
			req.VolumeCapabilities[0].AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		},
		"missing topology": func(req *csi.CreateVolumeRequest) {
			req.AccessibilityRequirements = nil
		},
		"topology without ShiftPV key": func(req *csi.CreateVolumeRequest) {
			req.AccessibilityRequirements.Preferred[0].Segments = map[string]string{"other": "worker-a"}
		},
		"unknown parameter": func(req *csi.CreateVolumeRequest) {
			req.Parameters["unknown"] = "value"
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			req := validCreateRequest("worker-a")
			mutate(req)
			service := &Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}}
			_, err := service.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestCreateVolumeRejectsUnconfiguredController(t *testing.T) {
	service := &Service{}
	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

func TestCreateVolumeReportsDirectoryFailure(t *testing.T) {
	operator := &fakeDirectoryOperator{createErr: errors.New("mkdir failed")}
	client := fake.NewClientset()
	service := configuredService(&Service{Client: client, Namespace: "shiftpv-system", Operator: operator})
	req := validCreateRequest("worker-a")

	_, err := service.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
	id, idErr := volume.IDFromName(req.Name)
	if idErr != nil {
		t.Fatal(idErr)
	}
	if _, getErr := client.CoreV1().ConfigMaps("shiftpv-system").Get(context.Background(), id, metav1.GetOptions{}); getErr != nil {
		t.Fatalf("expected reservation to remain retryable: %v", getErr)
	}
}

func TestCreateVolumeMapsRetryableDirectoryFailureToUnavailable(t *testing.T) {
	operator := &fakeDirectoryOperator{createErr: retryableDirectoryError{errors.New("filesystem unavailable")}}
	service := configuredService(&Service{Client: fake.NewClientset(), Namespace: "shiftpv-system", Operator: operator})

	_, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a"))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", err)
	}
}

func TestCreateVolumeRetriesAfterAmbiguousReservationTimeout(t *testing.T) {
	client := fake.NewClientset()
	operator := &fakeDirectoryOperator{}
	service := configuredService(&Service{Client: client, Namespace: "shiftpv-system", Operator: operator})
	req := validCreateRequest("worker-a")
	timedOut := false
	client.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if timedOut {
			return false, nil, nil
		}
		timedOut = true
		create := action.(k8stesting.CreateAction)
		cm := create.GetObject().(*corev1.ConfigMap).DeepCopy()
		if err := client.Tracker().Create(action.GetResource(), cm, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewTimeoutError("reservation response timed out", 1)
	})

	if _, err := service.CreateVolume(context.Background(), req); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected retryable Unavailable, got %v", err)
	}
	if operator.createCalls != 0 {
		t.Fatalf("directory operation ran after ambiguous reservation response: %d calls", operator.createCalls)
	}
	response, err := service.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetVolume().GetVolumeId() == "" || operator.createCalls != 1 {
		t.Fatalf("retry did not converge: response=%#v createCalls=%d", response, operator.createCalls)
	}
}

func TestCreateVolumePreservesDeadlineExceededCode(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	service := configuredService(&Service{Client: client, Namespace: "shiftpv-system", Operator: &fakeDirectoryOperator{}})

	if _, err := service.CreateVolume(context.Background(), validCreateRequest("worker-a")); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestKubernetesAPIErrorPreservesRetryAndContextCodes(t *testing.T) {
	tests := map[string]struct {
		err  error
		code codes.Code
	}{
		"canceled":          {err: context.Canceled, code: codes.Canceled},
		"deadline exceeded": {err: context.DeadlineExceeded, code: codes.DeadlineExceeded},
		"timeout":           {err: apierrors.NewTimeoutError("timed out", 1), code: codes.Unavailable},
		"throttled":         {err: apierrors.NewTooManyRequests("slow down", 1), code: codes.Unavailable},
		"unavailable":       {err: apierrors.NewServiceUnavailable("offline"), code: codes.Unavailable},
		"other":             {err: errors.New("invalid response"), code: codes.Internal},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := status.Code(kubernetesAPIError("call Kubernetes", test.err)); got != test.code {
				t.Fatalf("expected %s, got %s", test.code, got)
			}
		})
	}
}

func TestDeleteVolumeRejectsUnconfiguredController(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Service{}).DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

func TestControllerCapabilitiesAdvertiseCreateDeleteOnly(t *testing.T) {
	response, err := (&Service{}).ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Capabilities) != 1 || response.Capabilities[0].GetRpc().GetType() != csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME {
		t.Fatalf("unexpected capabilities: %#v", response.Capabilities)
	}
}

func TestValidateVolumeCapabilitiesReturnsMessageForUnsupportedMode(t *testing.T) {
	id, err := volume.IDFromName("pvc-uid")
	if err != nil {
		t.Fatal(err)
	}
	capability := validCreateRequest("worker-a").VolumeCapabilities[0]
	capability.AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	response, err := (&Service{}).ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           id,
		VolumeCapabilities: []*csi.VolumeCapability{capability},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Confirmed != nil || response.Message == "" {
		t.Fatalf("expected unsupported response message, got %#v", response)
	}
}

type rejectingProvisioningGate struct{}

func (rejectingProvisioningGate) Enter() (func(), error) {
	return nil, errors.New("quiescing")
}

func reservationForCreateRequest(id string, req *csi.CreateVolumeRequest) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: id, Namespace: "shiftpv-system",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "shiftpv",
				"app.kubernetes.io/component": "volume-reservation",
			},
		},
		Data: map[string]string{
			"requestName": req.Name,
			"volumeID":    id,
			"volumeUID":   "volume-uid",
			"nodeName":    req.AccessibilityRequirements.Preferred[0].Segments[TopologyKey],
			"capacity":    "67108864",
		},
	}
}

func validCreateRequest(node string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          "pvc-uid",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 64 << 20},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
		Parameters: map[string]string{},
		AccessibilityRequirements: &csi.TopologyRequirement{Preferred: []*csi.Topology{{
			Segments: map[string]string{TopologyKey: node},
		}}},
	}
}

func topologyNodes(topologies []*csi.Topology) []string {
	result := make([]string, 0, len(topologies))
	for _, topology := range topologies {
		result = append(result, topology.Segments[TopologyKey])
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
