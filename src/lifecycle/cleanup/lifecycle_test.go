package cleanup

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestValidateCheckRequest(t *testing.T) {
	for _, tc := range []struct {
		token, previous string
		valid           bool
	}{
		{"1", "", true}, {"2", "1", true}, {"18446744073709551615", "1", true},
		{"", "", false}, {"0", "", false}, {"01", "", false}, {"-1", "", false},
		{"18446744073709551616", "", false}, {"1", "1", false}, {"1", "2", false}, {"2", "invalid", false},
	} {
		t.Run(tc.token+"/"+tc.previous, func(t *testing.T) {
			if err := ValidateCheckRequest(tc.token, tc.previous); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestStartCheckRevalidatesChangedRequest(t *testing.T) {
	ctx := context.Background()
	j := Journal{Client: fake.NewClientset(), Namespace: "system"}
	i := fixture()
	if _, err := j.Ensure(ctx, i); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckRequest("2", ""); err != nil {
		t.Fatal(err)
	}
	cm, err := j.Client.CoreV1().ConfigMaps(j.Namespace).Get(ctx, Name(i.MoveUID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[CheckRequestKey] = "3"
	if _, err := j.Client.CoreV1().ConfigMaps(j.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := j.StartCheck(ctx, i, "2", "helper"); err == nil {
		t.Fatal("stale observation accepted at mutation")
	}
	record, err := j.Get(ctx, i.MoveUID)
	if err != nil || record.CheckID != "" || record.Completed {
		t.Fatalf("stale request mutated journal: %+v %v", record, err)
	}
}

func TestCheckAttemptPersistsAndRequiresFreshRequest(t *testing.T) {
	ctx := context.Background()
	j := Journal{Client: fake.NewClientset(), Namespace: "system"}
	i := fixture()
	if _, err := j.Ensure(ctx, i); err != nil {
		t.Fatal(err)
	}
	request := func(token string) {
		cm, _ := j.Client.CoreV1().ConfigMaps(j.Namespace).Get(ctx, Name(i.MoveUID), metav1.GetOptions{})
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		cm.Annotations[CheckRequestKey] = token
		if _, err := j.Client.CoreV1().ConfigMaps(j.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	request("1")
	if err := j.StartCheck(ctx, i, "1", "helper"); err != nil {
		t.Fatal(err)
	}
	if err := j.BindCheck(ctx, i, "1", "job-one"); err != nil {
		t.Fatal(err)
	}
	if err := j.BindCheck(ctx, i, "1", "impostor"); err == nil {
		t.Fatal("changed UID accepted")
	}
	request("2")
	if err := j.StartCheck(ctx, i, "2", "helper"); err == nil {
		t.Fatal("active check superseded")
	}
	if err := j.FinishCheck(ctx, i, "1", "job-one", false); err != nil {
		t.Fatal(err)
	}
	if err := j.StartCheck(ctx, i, "2", "helper"); err != nil {
		t.Fatal(err)
	}
	if err := j.BindCheck(ctx, i, "2", "job-two"); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishCheck(ctx, i, "2", "job-two", true); err != nil {
		t.Fatal(err)
	}
	r, err := j.Get(ctx, i.MoveUID)
	if err != nil || !r.Completed || r.CompletedAt.IsZero() || r.State != Completed {
		t.Fatalf("completion not durable: %+v %v", r, err)
	}
	if err := j.SetState(ctx, i, NeedsReview, "late observation"); err != nil {
		t.Fatal(err)
	}
	r, _ = j.Get(ctx, i.MoveUID)
	if r.State != Completed {
		t.Fatal("terminal state regressed")
	}
}

func TestCompletionTimestampIsStableAndInvalidMetadataFailsClosed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	j := Journal{Client: fake.NewClientset(), Namespace: "system", Now: func() time.Time { return now }}
	i := fixture()
	j.Ensure(ctx, i)
	j.BindJob(ctx, i, "job")
	if err := j.Complete(ctx, i, "job"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := j.Complete(ctx, i, "job"); err != nil {
		t.Fatal(err)
	}
	r, _ := j.Get(ctx, i.MoveUID)
	if !r.CompletedAt.Equal(now.Add(-time.Hour)) {
		t.Fatal("completion time extended on retry")
	}
	cm, _ := j.Client.CoreV1().ConfigMaps(j.Namespace).Get(ctx, Name(i.MoveUID), metav1.GetOptions{})
	cm.Annotations[completedAtKey] = "broken"
	if _, err := Decode(cm); err == nil {
		t.Fatal("invalid timestamp accepted")
	}
	cm.Annotations[completedAtKey] = ""
	cm.Annotations[StateKey] = "invented"
	if _, err := Decode(cm); err == nil {
		t.Fatal("invalid state accepted")
	}
}
