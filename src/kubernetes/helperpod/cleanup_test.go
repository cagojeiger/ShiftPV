package helperpod

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/volume"
)

func TestCleanupRunnerBindsExactJobAndWaitsForReceipt(t *testing.T) {
	ctx := context.Background()
	cleanups, cleanup := cleanupFixture(t)
	client := fake.NewClientset()
	var created *batchv1.Job
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		job.SetUID(types.UID("job-uid"))
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		created = job.DeepCopy()
		if err := client.Tracker().Add(job); err != nil {
			return true, nil, err
		}
		return true, job, nil
	})
	receiptWritten := false
	client.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !receiptWritten {
			current, err := cleanups.Get(ctx, cleanup.Name)
			if err != nil {
				return true, nil, err
			}
			receipt := &cleanupapi.Receipt{
				OperationID: current.Spec.OperationID, ExecutorUID: current.Status.Executor.JobUID,
				ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true,
			}
			if err := cleanups.UpdateStatus(ctx, current.Name, current.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: current.Status.Executor, Receipt: receipt}); err != nil {
				return true, nil, err
			}
			receiptWritten = true
		}
		return false, nil, nil
	})
	runner := validRunner(client)
	runner.ServiceAccountName = "shiftpv-controller"
	runner.PoolReadinessStaleAfter = 7 * time.Minute
	runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID, NodeName: cleanup.Spec.Target.NodeName, MountPath: "/mnt/shiftpv"})}
	result, err := runner.Reclaim(ctx, cleanup, cleanups)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.Phase != cleanupapi.PhaseVerifying || result.Status.Receipt == nil || created == nil {
		t.Fatalf("result=%#v job=%#v", result, created)
	}
	container := created.Spec.Template.Spec.Containers[0]
	if created.Name != cleanup.Name+"-effect" || created.Spec.Template.Spec.NodeName != cleanup.Spec.Target.NodeName ||
		created.Spec.Template.Spec.ServiceAccountName != "shiftpv-controller" || container.Command[0] != "/shiftpv-volume-helper" ||
		!strings.Contains(strings.Join(container.Args, " "), "--cleanup-uid="+cleanup.UID) ||
		!strings.Contains(strings.Join(container.Args, " "), "--pool-readiness-stale-after=7m0s") {
		t.Fatalf("cleanup Job identity=%#v", created)
	}
	started, err := client.BatchV1().Jobs(runner.Namespace).Get(ctx, created.Name, metav1.GetOptions{})
	if err != nil || started.Spec.Suspend == nil || *started.Spec.Suspend {
		t.Fatalf("bound cleanup Job was not started: suspend=%v err=%v", started.Spec.Suspend, err)
	}
	if second, err := runner.Reclaim(ctx, result, cleanups); err != nil || second.Status.Phase != cleanupapi.PhaseVerifying {
		t.Fatalf("retry=%#v err=%v", second, err)
	}
}

func TestCleanupRunnerRejectsChangedPoolAndJob(t *testing.T) {
	cleanups, cleanup := cleanupFixture(t)
	runner := validRunner(fake.NewClientset())
	runner.Pools = fakePoolResolver{pool: volumeapi.Pool{Name: "replacement", UID: cleanup.Spec.Target.PoolUID, MountPath: "/mnt/shiftpv"}}
	if _, err := runner.Reclaim(context.Background(), cleanup, cleanups); err == nil || !strings.Contains(err.Error(), "PoolIdentityChanged") {
		t.Fatalf("replacement Pool accepted: %v", err)
	}
	if current, err := cleanups.Get(context.Background(), cleanup.Name); err != nil || current.Status.Phase != cleanupapi.PhaseNeedsReview || current.Status.Reason != "PoolIdentityChanged" {
		t.Fatalf("Pool mismatch did not converge to review: %#v err=%v", current, err)
	}

	cleanups, cleanup = cleanupFixture(t)
	runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID, MountPath: "/mnt/shiftpv"})}
	wanted := runner.cleanupJob(cleanup, "/mnt/shiftpv")
	wanted.UID = "foreign-uid"
	wanted.Spec.Template.Spec.NodeName = "other-node"
	runner.Client = fake.NewClientset(wanted)
	if _, err := runner.Reclaim(context.Background(), cleanup, cleanups); err == nil || !strings.Contains(err.Error(), "JobIdentityChanged") {
		t.Fatalf("changed Job accepted: %v", err)
	}
}

func TestCleanupRunnerRejectsExecutorStartedBeforeUIDBinding(t *testing.T) {
	cleanups, cleanup := cleanupFixture(t)
	runner := validRunner(fake.NewClientset())
	runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{
		Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID,
		NodeName: cleanup.Spec.Target.NodeName, MountPath: "/mnt/shiftpv",
	})}
	job := runner.cleanupJob(cleanup, "/mnt/shiftpv")
	job.UID = "job-uid"
	job.Spec.Suspend = boolPtr(false)
	runner.Client = fake.NewClientset(job)
	if _, err := runner.Reclaim(context.Background(), cleanup, cleanups); err == nil || !strings.Contains(err.Error(), "ExecutorStartedBeforeBinding") {
		t.Fatalf("unbound executor was accepted: %v", err)
	}
	current, err := cleanups.Get(context.Background(), cleanup.Name)
	if err != nil || current.Status.Phase != cleanupapi.PhaseNeedsReview || current.Status.Reason != "ExecutorStartedBeforeBinding" {
		t.Fatalf("unbound executor did not converge to review: %#v err=%v", current, err)
	}
}

func TestCleanupRunnerDefersEffectWhilePoolIsNotReady(t *testing.T) {
	cleanups, cleanup := cleanupFixture(t)
	client := fake.NewClientset()
	runner := validRunner(client)
	runner.Pools = fakePoolResolver{pool: volumeapi.Pool{
		Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID,
		NodeName: cleanup.Spec.Target.NodeName, MountPath: "/mnt/shiftpv",
	}}
	_, err := runner.Reclaim(context.Background(), cleanup, cleanups)
	if err == nil || !isRetryable(err) {
		t.Fatalf("unready Pool was not deferred retryably: %v", err)
	}
	current, getErr := cleanups.Get(context.Background(), cleanup.Name)
	jobs, listErr := client.BatchV1().Jobs(runner.Namespace).List(context.Background(), metav1.ListOptions{})
	if getErr != nil || listErr != nil || current.Status.Phase != "" || len(jobs.Items) != 0 {
		t.Fatalf("unready Pool changed cleanup or started effect: cleanup=%#v jobs=%#v getErr=%v listErr=%v", current, jobs.Items, getErr, listErr)
	}
}

func TestCleanupRunnerJoinsStartedExecutorFromStalePendingSnapshot(t *testing.T) {
	ctx := context.Background()
	cleanups, stale := cleanupFixture(t)
	runner := validRunner(fake.NewClientset())
	runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{
		Name: stale.Spec.Target.PoolName, UID: stale.Spec.Target.PoolUID,
		NodeName: stale.Spec.Target.NodeName, MountPath: "/mnt/shiftpv",
	})}
	job := runner.cleanupJob(stale, "/mnt/shiftpv")
	job.UID = "job-uid"
	job.Spec.Suspend = boolPtr(false)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	client := fake.NewClientset(job)
	runner.Client = client
	executor := &cleanupapi.Executor{JobName: job.Name, JobUID: string(job.UID), NodeName: stale.Spec.Target.NodeName}
	if err := cleanups.UpdateStatus(ctx, stale.Name, stale.UID, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	receiptWritten := false
	client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !receiptWritten {
			receipt := &cleanupapi.Receipt{
				OperationID: stale.Spec.OperationID, ExecutorUID: executor.JobUID,
				ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true,
			}
			if err := cleanups.UpdateStatus(ctx, stale.Name, stale.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: executor, Receipt: receipt}); err != nil {
				return true, nil, err
			}
			receiptWritten = true
		}
		return false, nil, nil
	})
	result, err := runner.Reclaim(ctx, stale, cleanups)
	if err != nil || result.Status.Phase != cleanupapi.PhaseVerifying {
		t.Fatalf("stale reconciler did not join bound executor: result=%#v err=%v", result, err)
	}
}

func TestCleanupRunnerWaitsForJobTerminationAfterReceipt(t *testing.T) {
	ctx := context.Background()
	cleanups, cleanup := cleanupFixture(t)
	runner := validRunner(nil)
	runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{
		Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID,
		NodeName: cleanup.Spec.Target.NodeName, MountPath: "/mnt/shiftpv",
	})}
	job := runner.cleanupJob(cleanup, "/mnt/shiftpv")
	job.UID = "job-uid"
	job.Spec.Suspend = boolPtr(false)
	client := fake.NewClientset(job)
	runner.Client = client
	executor := &cleanupapi.Executor{JobName: job.Name, JobUID: string(job.UID), NodeName: cleanup.Spec.Target.NodeName}
	receipt := &cleanupapi.Receipt{
		OperationID: cleanup.Spec.OperationID, ExecutorUID: executor.JobUID,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true,
	}
	if err := cleanups.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	if err := cleanups.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: executor, Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	runner.Timeout = 20 * time.Millisecond
	if _, err := runner.Reclaim(ctx, cleanup, cleanups); !isRetryable(err) {
		t.Fatalf("receipt without executor termination was accepted: %v", err)
	}
	current, err := client.BatchV1().Jobs(runner.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := client.BatchV1().Jobs(runner.Namespace).UpdateStatus(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	runner.Timeout = time.Second
	result, err := runner.Reclaim(ctx, cleanup, cleanups)
	if err != nil || result.Status.Phase != cleanupapi.PhaseVerifying {
		t.Fatalf("terminated executor did not settle receipt: result=%#v err=%v", result, err)
	}
}

func TestCleanupRunnerAcceptsVerifyingReceiptAfterTTLRemovedExecutor(t *testing.T) {
	ctx := context.Background()
	cleanups, cleanup := cleanupFixture(t)
	executor := &cleanupapi.Executor{JobName: cleanup.Name + "-effect", JobUID: "job-uid", NodeName: cleanup.Spec.Target.NodeName}
	receipt := &cleanupapi.Receipt{
		OperationID: cleanup.Spec.OperationID, ExecutorUID: executor.JobUID,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true,
	}
	if err := cleanups.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	if err := cleanups.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: executor, Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset()
	createCalls := 0
	client.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		createCalls++
		return false, nil, nil
	})
	runner := validRunner(client)
	runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{
		Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID,
		NodeName: cleanup.Spec.Target.NodeName, MountPath: "/mnt/shiftpv",
	})}
	result, err := runner.Reclaim(ctx, cleanup, cleanups)
	if err != nil || result.Status.Phase != cleanupapi.PhaseVerifying || createCalls != 0 {
		t.Fatalf("TTL-settled executor did not converge: result=%#v createCalls=%d err=%v", result, createCalls, err)
	}
}

func TestCleanupRunnerWaitsForTTLOwnedPodAfterJobDisappears(t *testing.T) {
	ctx := context.Background()
	cleanups, cleanup := cleanupFixture(t)
	executor := &cleanupapi.Executor{JobName: cleanup.Name + "-effect", JobUID: "job-uid", NodeName: cleanup.Spec.Target.NodeName}
	receipt := &cleanupapi.Receipt{
		OperationID: cleanup.Spec.OperationID, ExecutorUID: executor.JobUID,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true,
	}
	if err := cleanups.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	if err := cleanups.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: executor, Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "terminating-helper", Namespace: "shiftpv-system",
		Labels:          map[string]string{cleanupNameLabel: cleanup.Name, cleanupUIDLabel: cleanup.UID},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", UID: "job-uid"}},
	}}
	client := fake.NewClientset(pod)
	runner := validRunner(client)
	runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{
		Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID,
		NodeName: cleanup.Spec.Target.NodeName, MountPath: "/mnt/shiftpv",
	})}
	runner.Timeout = 20 * time.Millisecond
	if _, err := runner.Reclaim(ctx, cleanup, cleanups); !isRetryable(err) {
		t.Fatalf("live TTL-owned Pod was treated as settled: %v", err)
	}
	if err := client.CoreV1().Pods(runner.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	runner.Timeout = time.Second
	result, err := runner.Reclaim(ctx, cleanup, cleanups)
	if err != nil || result.Status.Phase != cleanupapi.PhaseVerifying {
		t.Fatalf("removed TTL-owned Pod did not settle: result=%#v err=%v", result, err)
	}
}

func TestCleanupRunnerRejectsIncompleteVerifyingReceipt(t *testing.T) {
	ctx := context.Background()
	cleanups, cleanup := cleanupFixture(t)
	executor := &cleanupapi.Executor{JobName: cleanup.Name + "-effect", JobUID: "job-uid", NodeName: cleanup.Spec.Target.NodeName}
	receipt := &cleanupapi.Receipt{
		OperationID: cleanup.Spec.OperationID, ExecutorUID: executor.JobUID,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true,
	}
	if err := cleanups.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseRunning, Executor: executor}); err != nil {
		t.Fatal(err)
	}
	if err := cleanups.UpdateStatus(ctx, cleanup.Name, cleanup.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: executor, Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	runner := validRunner(fake.NewClientset())
	runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{
		Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID,
		NodeName: cleanup.Spec.Target.NodeName, MountPath: "/mnt/shiftpv",
	})}
	if _, err := runner.Reclaim(ctx, cleanup, cleanups); err == nil {
		t.Fatal("incomplete Verifying receipt was accepted")
	}
	current, err := cleanups.Get(ctx, cleanup.Name)
	if err != nil || current.Status.Phase != cleanupapi.PhaseNeedsReview || current.Status.Reason != "ReceiptInvalid" {
		t.Fatalf("current=%#v err=%v", current, err)
	}
}

func TestOwnedByJobRequiresExactControllerUID(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{
		{APIVersion: "batch/v1", Kind: "Job", UID: "job-uid"},
	}}}
	if !ownedByJob(pod, "job-uid") {
		t.Fatal("exact Job owner was not recognized")
	}
	if ownedByJob(pod, "replacement-job-uid") {
		t.Fatal("replacement Job owner was accepted")
	}
	pod.OwnerReferences[0].Kind = "Pod"
	if ownedByJob(pod, "job-uid") {
		t.Fatal("non-Job owner was accepted")
	}
}

func TestCleanupRunnerReacquiresJobAfterAcceptedCreateResponseLoss(t *testing.T) {
	ctx := context.Background()
	cleanups, cleanup := cleanupFixture(t)
	client := fake.NewClientset()
	createCalls := 0
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		job.UID = "job-uid"
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		if err := client.Tracker().Add(job); err != nil {
			return true, nil, err
		}
		createCalls++
		return true, nil, apierrors.NewTimeoutError("accepted cleanup Job, response lost", 1)
	})
	receiptWritten := false
	client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		current, err := cleanups.Get(ctx, cleanup.Name)
		if err == nil && current.Status.Phase == cleanupapi.PhaseRunning && !receiptWritten {
			receipt := &cleanupapi.Receipt{
				OperationID: current.Spec.OperationID, ExecutorUID: current.Status.Executor.JobUID,
				ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Retired: true, Purged: true,
			}
			if err := cleanups.UpdateStatus(ctx, current.Name, current.UID, cleanupapi.Status{Phase: cleanupapi.PhaseVerifying, Executor: current.Status.Executor, Receipt: receipt}); err != nil {
				return true, nil, err
			}
			receiptWritten = true
		}
		return false, nil, nil
	})
	runner := validRunner(client)
	runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{
		Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID,
		NodeName: cleanup.Spec.Target.NodeName, MountPath: "/mnt/shiftpv",
	})}
	result, err := runner.Reclaim(ctx, cleanup, cleanups)
	if err != nil || result.Status.Phase != cleanupapi.PhaseVerifying || createCalls != 1 {
		t.Fatalf("accepted cleanup Job was not reacquired: result=%#v creates=%d err=%v", result, createCalls, err)
	}
}

func TestSameCleanupJobAcceptsAPIDefaultsButRejectsEffectChanges(t *testing.T) {
	cleanups, cleanup := cleanupFixture(t)
	_ = cleanups
	runner := &Runner{Namespace: "shiftpv-system", ServiceAccountName: "shiftpv-controller", Image: "helper:test", Timeout: 500 * time.Millisecond}
	expected := runner.cleanupJob(cleanup, "/mnt/shiftpv")
	if expected.Spec.Suspend == nil || !*expected.Spec.Suspend {
		t.Fatal("cleanup Job must be suspended until its UID is durable")
	}
	current := expected.DeepCopy()
	fieldRef := current.Spec.Template.Spec.Containers[0].Env[0].ValueFrom.FieldRef
	if fieldRef == nil || fieldRef.APIVersion != "v1" || fieldRef.FieldPath != "metadata.name" {
		t.Fatalf("cleanup executor identity env=%#v", fieldRef)
	}
	current.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	current.Spec.Template.Spec.SchedulerName = corev1.DefaultSchedulerName
	terminationGrace := int64(30)
	current.Spec.Template.Spec.TerminationGracePeriodSeconds = &terminationGrace
	if !sameCleanupJob(current, expected) {
		t.Fatal("Kubernetes API default fields changed cleanup identity")
	}
	current.Spec.Template.Spec.Containers[0].Args = append(current.Spec.Template.Spec.Containers[0].Args, "--pool-root=/other")
	if sameCleanupJob(current, expected) {
		t.Fatal("changed cleanup effect was accepted")
	}
	parallel := int32(2)
	current = expected.DeepCopy()
	current.Spec.Parallelism = &parallel
	if sameCleanupJob(current, expected) {
		t.Fatal("parallel cleanup effect was accepted")
	}
	short := runner.cleanupJob(cleanup, "/mnt/shiftpv")
	if short.Spec.ActiveDeadlineSeconds == nil || *short.Spec.ActiveDeadlineSeconds != 1 {
		t.Fatalf("sub-second timeout deadline = %v", short.Spec.ActiveDeadlineSeconds)
	}
}

func TestCleanupRunnerReportsFailedAndReceiptlessJobs(t *testing.T) {
	for name, condition := range map[string]batchv1.JobConditionType{
		"failed": batchv1.JobFailed, "receiptless": batchv1.JobComplete,
	} {
		t.Run(name, func(t *testing.T) {
			cleanups, cleanup := cleanupFixture(t)
			client := fake.NewClientset()
			client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
				job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
				job.SetUID("job-uid")
				job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue, Message: name}}
				_ = client.Tracker().Add(job)
				return true, job, nil
			})
			runner := validRunner(client)
			runner.Pools = fakePoolResolver{pool: readyCleanupPool(volumeapi.Pool{Name: cleanup.Spec.Target.PoolName, UID: cleanup.Spec.Target.PoolUID, MountPath: "/mnt/shiftpv"})}
			if _, err := runner.Reclaim(context.Background(), cleanup, cleanups); err == nil {
				t.Fatal("terminal Job without receipt accepted")
			}
			current, err := cleanups.Get(context.Background(), cleanup.Name)
			if err != nil || current.Status.Phase != cleanupapi.PhaseNeedsReview {
				t.Fatalf("terminal Job did not converge to review: %#v err=%v", current, err)
			}
			wantReason := "ExecutorFailed"
			if condition == batchv1.JobComplete {
				wantReason = "ReceiptMissing"
			}
			if current.Status.Reason != wantReason {
				t.Fatalf("review reason=%q want=%q", current.Status.Reason, wantReason)
			}
		})
	}
}

func cleanupFixture(t *testing.T) (*cleanupapi.Store, cleanupapi.Cleanup) {
	t.Helper()
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{cleanupapi.Resource: "ShiftPVCleanupList"})
	dynamicClient.PrependReactor("create", "shiftpvcleanups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID("cleanup-uid")
		return false, nil, nil
	})
	store := &cleanupapi.Store{Client: dynamicClient}
	target := volume.CopyIdentity{
		InstallationID: "installation", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: testVolumeID, VolumeUID: "volume-uid", CopyID: "copy-id",
		NodeName: "worker-a", Role: volume.RoleServing,
	}
	cleanup, err := store.Ensure(context.Background(), cleanupapi.Spec{
		OperationID: "delete-volume-uid", Target: target, Reason: "VolumeDelete",
		Approved:  true,
		Authority: cleanupapi.Authority{Kind: "ShiftPVVolume", Name: target.VolumeID, UID: target.VolumeUID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, cleanup
}

func readyCleanupPool(pool volumeapi.Pool) volumeapi.Pool {
	now := metav1.Now()
	pool.Generation = 1
	pool.Status = volumeapi.PoolStatus{
		ObservedGeneration: 1,
		LastProbeTime:      now,
		Conditions: []metav1.Condition{{
			Type: volumeapi.PoolConditionReady, Status: metav1.ConditionTrue,
			ObservedGeneration: 1, LastTransitionTime: now, Reason: "PoolReady",
		}},
	}
	return pool
}
