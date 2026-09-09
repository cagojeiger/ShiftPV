package cleanup

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

const (
	Pending         = "Pending"
	Running         = "Running"
	NeedsReview     = "NeedsReview"
	Completed       = "Completed"
	StateKey        = "shiftpv.io/cleanup-state"
	ReasonKey       = "shiftpv.io/cleanup-reason"
	CheckRequestKey = "shiftpv.io/cleanup-check"
	completedAtKey  = "shiftpv.io/cleanup-completed-at"
	checkIDKey      = "shiftpv.io/cleanup-check-id"
	checkUIDKey     = "shiftpv.io/cleanup-check-job-uid"
	checkDoneKey    = "shiftpv.io/cleanup-check-done"
	checkImageKey   = "shiftpv.io/cleanup-check-image"
)

func checkSequence(value string) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != value {
		return 0, fmt.Errorf("cleanup check must be a positive increasing integer")
	}
	return n, nil
}

// ValidateCheckRequest validates a new sequence before authority lookups.
// StartCheck repeats it against the current journal at the mutation boundary.
func ValidateCheckRequest(token, previous string) error {
	next, err := checkSequence(token)
	if err != nil {
		return err
	}
	var prior uint64
	if previous != "" {
		prior, err = checkSequence(previous)
		if err != nil {
			return err
		}
	}
	if next <= prior {
		return fmt.Errorf("cleanup check number must increase")
	}
	return nil
}

func decodeLifecycle(cm *corev1.ConfigMap, r *Record) error {
	r.UID, r.ResourceVersion = string(cm.UID), cm.ResourceVersion
	r.State, r.Reason = cm.Annotations[StateKey], cm.Annotations[ReasonKey]
	switch r.State {
	case "":
		r.State = Pending
	case Pending, Running, NeedsReview, Completed:
	default:
		return fmt.Errorf("invalid cleanup state")
	}
	if r.State == Completed && !r.Completed {
		return fmt.Errorf("cleanup state lacks completion evidence")
	}
	if r.Completed {
		r.State = Completed
	}
	if value := cm.Annotations[completedAtKey]; value != "" {
		var err error
		r.CompletedAt, err = time.Parse(time.RFC3339Nano, value)
		if err != nil || !r.Completed {
			return fmt.Errorf("invalid cleanup completion timestamp")
		}
	}
	r.CheckRequest, r.CheckID, r.CheckJobUID = cm.Annotations[CheckRequestKey], cm.Annotations[checkIDKey], cm.Annotations[checkUIDKey]
	r.CheckImage = cm.Annotations[checkImageKey]
	if r.CheckID != "" && r.CheckImage == "" {
		return fmt.Errorf("cleanup check image missing")
	}
	if r.CheckID != "" {
		if _, err := checkSequence(r.CheckID); err != nil {
			return err
		}
	}
	switch cm.Annotations[checkDoneKey] {
	case "":
	case "true":
		r.CheckDone = true
	default:
		return fmt.Errorf("invalid cleanup check result")
	}
	if (r.CheckJobUID != "" || r.CheckDone) && r.CheckID == "" {
		return fmt.Errorf("cleanup check identity missing")
	}
	if r.Completed && r.JobUID == "" && (!r.CheckDone || r.CheckJobUID == "") {
		return fmt.Errorf("cleanup completion lacks verified check")
	}
	return nil
}

func (j Journal) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j Journal) markComplete(cm *corev1.ConfigMap) {
	cm.Annotations[completedKey] = "true"
	cm.Annotations[StateKey] = Completed
	delete(cm.Annotations, ReasonKey)
	if cm.Annotations[completedAtKey] == "" {
		cm.Annotations[completedAtKey] = j.now().UTC().Format(time.RFC3339Nano)
	}
}

func (j Journal) mutate(ctx context.Context, i Intent, fn func(*corev1.ConfigMap, Record) error) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := j.Client.CoreV1().ConfigMaps(j.Namespace).Get(ctx, Name(i.MoveUID), metav1.GetOptions{})
		if err != nil {
			return err
		}
		r, err := Decode(cm)
		if err != nil {
			return err
		}
		if r.Intent != i || cm.DeletionTimestamp != nil {
			return fmt.Errorf("cleanup request identity changed")
		}
		before := cm.DeepCopy()
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		if err := fn(cm, r); err != nil {
			return err
		}
		// Reconcile observations only write when their content changes.
		if equalAnnotations(before.Annotations, cm.Annotations) {
			return nil
		}
		_, err = j.Client.CoreV1().ConfigMaps(j.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func equalAnnotations(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if other, ok := b[k]; !ok || other != v {
			return false
		}
	}
	return true
}

func (j Journal) SetState(ctx context.Context, i Intent, state, reason string) error {
	if state != Pending && state != Running && state != NeedsReview {
		return fmt.Errorf("invalid nonterminal cleanup state")
	}
	return j.mutate(ctx, i, func(cm *corev1.ConfigMap, r Record) error {
		if r.Completed {
			j.markComplete(cm)
			return nil
		}
		cm.Annotations[StateKey], cm.Annotations[ReasonKey] = state, reason
		return nil
	})
}

func (j Journal) StartCheck(ctx context.Context, i Intent, token, image string) error {
	if image == "" {
		return fmt.Errorf("cleanup check image required")
	}
	if err := ValidateCheckRequest(token, ""); err != nil {
		return err
	}
	return j.mutate(ctx, i, func(cm *corev1.ConfigMap, r Record) error {
		if r.Completed {
			return nil
		}
		if r.CheckID == token {
			return nil
		}
		if err := ValidateCheckRequest(token, r.CheckID); err != nil {
			return err
		}
		if r.CheckID != "" && !r.CheckDone {
			return fmt.Errorf("previous cleanup check is still active")
		}
		if r.CheckRequest != token {
			return fmt.Errorf("cleanup check request changed")
		}
		cm.Annotations[checkIDKey] = token
		cm.Annotations[checkImageKey] = image
		delete(cm.Annotations, checkUIDKey)
		delete(cm.Annotations, checkDoneKey)
		cm.Annotations[StateKey], cm.Annotations[ReasonKey] = Running, "Checking source paths"
		return nil
	})
}

func (j Journal) BindCheck(ctx context.Context, i Intent, token, uid string) error {
	if uid == "" {
		return fmt.Errorf("cleanup check Job UID is required")
	}
	return j.mutate(ctx, i, func(cm *corev1.ConfigMap, r Record) error {
		if r.CheckID != token || (r.CheckJobUID != "" && r.CheckJobUID != uid) {
			return fmt.Errorf("cleanup check identity changed")
		}
		if r.CheckDone || r.Completed {
			return nil
		}
		cm.Annotations[checkUIDKey] = uid
		return nil
	})
}

func (j Journal) FinishCheck(ctx context.Context, i Intent, token, uid string, success bool) error {
	return j.mutate(ctx, i, func(cm *corev1.ConfigMap, r Record) error {
		if token == "" || r.CheckID != token || r.CheckJobUID != uid || (success && uid == "") {
			return fmt.Errorf("cleanup check identity changed")
		}
		if r.CheckDone || r.Completed {
			return nil
		}
		cm.Annotations[checkDoneKey] = "true"
		if success {
			j.markComplete(cm)
		} else {
			cm.Annotations[StateKey], cm.Annotations[ReasonKey] = NeedsReview, "Check failed or disappeared; inspect paths and submit a new check token"
		}
		return nil
	})
}

func (j Journal) DeleteCompleted(ctx context.Context, r Record, before time.Time) error {
	if !r.Completed || r.CompletedAt.IsZero() || r.CompletedAt.After(before) || r.UID == "" || r.ResourceVersion == "" {
		return fmt.Errorf("cleanup evidence is not eligible for retention expiry")
	}
	uid := types.UID(r.UID)
	return j.Client.CoreV1().ConfigMaps(j.Namespace).Delete(ctx, Name(r.Intent.MoveUID), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &r.ResourceVersion}})
}
