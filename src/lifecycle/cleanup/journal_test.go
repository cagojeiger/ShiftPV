package cleanup

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func fixture() Intent {
	return Intent{MoveName: "move-a", MoveUID: "uid-a", VolumeID: "shiftpv-a", SourceNode: "source", DestinationNode: "destination", PoolPath: "/var/lib/shiftpv", JobName: "cleanup-a"}
}

func TestJournalSurvivesRestartAndRejectsChangedIntent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	j := Journal{Client: client, Namespace: "system"}
	intent := fixture()
	if _, err := j.Ensure(ctx, intent); err != nil {
		t.Fatal(err)
	}
	j = Journal{Client: client, Namespace: "system"}
	record, err := j.Ensure(ctx, intent)
	if err != nil || record.Completed {
		t.Fatalf("read pending: %+v %v", record, err)
	}
	cm, err := client.CoreV1().ConfigMaps("system").Get(ctx, Name(intent.MoveUID), metav1.GetOptions{})
	if err != nil || cm.Immutable == nil || !*cm.Immutable || len(cm.OwnerReferences) != 0 {
		t.Fatalf("intent not durable/immutable: %+v %v", cm, err)
	}
	for _, change := range []func(*Intent){func(i *Intent) { i.PoolPath = "/other" }, func(i *Intent) { i.VolumeID = "other" }, func(i *Intent) { i.DestinationNode = "other" }} {
		changed := intent
		change(&changed)
		if _, err := j.Ensure(ctx, changed); err == nil {
			t.Fatal("changed intent accepted")
		}
	}
	if err := j.BindJob(ctx, intent, "job-uid"); err != nil {
		t.Fatal(err)
	}
	if err := j.BindJob(ctx, intent, "replacement-job"); err == nil {
		t.Fatal("recreated Job accepted")
	}
	if err := j.Complete(ctx, intent, "other-job"); err == nil {
		t.Fatal("foreign evidence accepted")
	}
	if err := j.Complete(ctx, intent, "job-uid"); err != nil {
		t.Fatal(err)
	}
	if err := j.Complete(ctx, intent, "job-uid"); err != nil {
		t.Fatal(err)
	}
	record, err = j.Get(ctx, intent.MoveUID)
	if err != nil || !record.Completed || record.JobUID != "job-uid" {
		t.Fatalf("lost acknowledgement: %+v %v", record, err)
	}
}

func TestJournalHandlesAcceptedCreateAndUpdateResponseLoss(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	j := Journal{Client: client, Namespace: "system"}
	intent := fixture()
	client.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cm := action.(ktesting.CreateAction).GetObject().(*corev1.ConfigMap)
		if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, cm.Namespace); err != nil {
			return true, nil, err
		}
		return true, nil, errors.New("response lost")
	})
	if _, err := j.Ensure(ctx, intent); err == nil {
		t.Fatal("write error hidden")
	}
	if _, err := j.Ensure(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := j.BindJob(ctx, intent, "job-uid"); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("update", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cm := action.(ktesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, cm.Namespace); err != nil {
			return true, nil, err
		}
		return true, nil, errors.New("response lost")
	})
	if err := j.Complete(ctx, intent, "job-uid"); err == nil {
		t.Fatal("write error hidden")
	}
	if err := j.Complete(ctx, intent, "job-uid"); err != nil {
		t.Fatal(err)
	}
}

func TestJournalRejectsInvalidOrCorruptRequests(t *testing.T) {
	for _, change := range []func(*Intent){
		func(i *Intent) { i.MoveUID = "" }, func(i *Intent) { i.VolumeID = "../data" },
		func(i *Intent) { i.MoveName = ".." }, func(i *Intent) { i.PoolPath = "/" },
		func(i *Intent) { i.PoolPath = "relative" }, func(i *Intent) { i.SourceNode = i.DestinationNode },
	} {
		i := fixture()
		change(&i)
		if _, err := (Journal{Client: fake.NewClientset(), Namespace: "system"}).Ensure(context.Background(), i); err == nil {
			t.Fatalf("unsafe request accepted: %+v", i)
		}
	}
	for _, change := range []func(*corev1.ConfigMap){
		func(cm *corev1.ConfigMap) { cm.Immutable = nil }, func(cm *corev1.ConfigMap) { cm.Labels[Label] = "unknown" },
		func(cm *corev1.ConfigMap) { cm.Data["intent"] = "{" }, func(cm *corev1.ConfigMap) { cm.Name = "wrong" },
		func(cm *corev1.ConfigMap) { cm.Annotations = map[string]string{completedKey: "true"} }, func(cm *corev1.ConfigMap) { cm.Annotations = map[string]string{completedKey: "unknown"} },
	} {
		ctx := context.Background()
		client := fake.NewClientset()
		j := Journal{Client: client, Namespace: "system"}
		if _, err := j.Ensure(ctx, fixture()); err != nil {
			t.Fatal(err)
		}
		cm, err := client.CoreV1().ConfigMaps("system").Get(ctx, Name(fixture().MoveUID), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		change(cm)
		if _, err := Decode(cm); err == nil {
			t.Fatal("corrupt request accepted")
		}
	}
}

func TestJournalConflictPreservesOtherAttempt(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	j := Journal{Client: client, Namespace: "system"}
	intent := fixture()
	if _, err := j.Ensure(ctx, intent); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("update", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cm := action.(ktesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		cm.Annotations[jobUIDKey] = "other-attempt"
		if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, cm.Namespace); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), cm.Name, errors.New("concurrent binding"))
	})
	if err := j.BindJob(ctx, intent, "my-attempt"); err == nil {
		t.Fatal("conflicting attempt overwritten")
	}
	record, err := j.Get(ctx, intent.MoveUID)
	if err != nil || record.JobUID != "other-attempt" || record.Completed {
		t.Fatalf("concurrent identity lost: %+v %v", record, err)
	}
}
