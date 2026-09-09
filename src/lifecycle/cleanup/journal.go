package cleanup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	Label        = "shiftpv.io/cleanup-request"
	jobUIDKey    = "shiftpv.io/cleanup-job-uid"
	completedKey = "shiftpv.io/cleanup-completed"
)

type Intent struct {
	MoveName        string `json:"moveName"`
	MoveUID         string `json:"moveUID"`
	VolumeID        string `json:"volumeID"`
	SourceNode      string `json:"sourceNode"`
	DestinationNode string `json:"destinationNode"`
	PoolPath        string `json:"poolPath"`
	JobName         string `json:"jobName"`
}

type Record struct {
	Intent                             Intent
	JobUID                             string
	Completed                          bool
	UID, ResourceVersion               string
	State, Reason                      string
	CompletedAt                        time.Time
	CheckRequest, CheckID, CheckJobUID string
	CheckImage                         string
	CheckDone                          bool
}

type Journal struct {
	Client    kubernetes.Interface
	Namespace string
	Now       func() time.Time
}

func Name(moveUID string) string {
	sum := sha256.Sum256([]byte(moveUID))
	return "shiftpv-cleanup-" + hex.EncodeToString(sum[:16])
}

func (i Intent) Validate() error {
	if i.MoveUID == "" || i.MoveName == "" || i.VolumeID == "" || i.SourceNode == "" || i.DestinationNode == "" || i.SourceNode == i.DestinationNode || i.JobName == "" || !filepath.IsAbs(i.PoolPath) || filepath.Clean(i.PoolPath) != i.PoolPath || i.PoolPath == "/" {
		return fmt.Errorf("incomplete or unsafe source cleanup intent")
	}
	for _, part := range []string{i.MoveName, i.VolumeID} {
		if part == "." || part == ".." || filepath.Base(part) != part {
			return fmt.Errorf("invalid cleanup dataset identity")
		}
	}
	return nil
}

func (j Journal) Get(ctx context.Context, moveUID string) (Record, error) {
	cm, err := j.Client.CoreV1().ConfigMaps(j.Namespace).Get(ctx, Name(moveUID), metav1.GetOptions{})
	if err != nil {
		return Record{}, err
	}
	record, err := Decode(cm)
	if err == nil && record.Intent.MoveUID != moveUID {
		err = fmt.Errorf("cleanup request identity changed")
	}
	return record, err
}

func Decode(cm *corev1.ConfigMap) (Record, error) {
	var r Record
	if cm.Labels[Label] != "source-v1" || cm.Immutable == nil || !*cm.Immutable {
		return r, fmt.Errorf("unrecognized cleanup request %q", cm.Name)
	}
	if err := json.Unmarshal([]byte(cm.Data["intent"]), &r.Intent); err != nil {
		return r, fmt.Errorf("decode cleanup request %q: %w", cm.Name, err)
	}
	if err := r.Intent.Validate(); err != nil {
		return r, err
	}
	if cm.Name != Name(r.Intent.MoveUID) {
		return r, fmt.Errorf("cleanup request name/identity mismatch")
	}
	r.JobUID = cm.Annotations[jobUIDKey]
	switch cm.Annotations[completedKey] {
	case "":
	case "true":
		if r.JobUID == "" && cm.Annotations[checkUIDKey] == "" {
			return r, fmt.Errorf("cleanup acknowledgement lacks Job identity")
		}
		r.Completed = true
	default:
		return r, fmt.Errorf("invalid cleanup acknowledgement")
	}
	if err := decodeLifecycle(cm, &r); err != nil {
		return r, err
	}
	return r, nil
}

func (j Journal) Ensure(ctx context.Context, intent Intent) (Record, error) {
	if err := intent.Validate(); err != nil {
		return Record{}, err
	}
	r, err := j.Get(ctx, intent.MoveUID)
	if apierrors.IsNotFound(err) {
		data, encodeErr := json.Marshal(intent)
		if encodeErr != nil {
			return Record{}, encodeErr
		}
		immutable := true
		_, err = j.Client.CoreV1().ConfigMaps(j.Namespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: Name(intent.MoveUID), Namespace: j.Namespace, Labels: map[string]string{Label: "source-v1"}},
			Immutable:  &immutable, Data: map[string]string{"intent": string(data)},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return Record{}, err
		}
		r, err = j.Get(ctx, intent.MoveUID)
	}
	if err == nil && r.Intent != intent {
		err = fmt.Errorf("cleanup request differs from approved intent")
	}
	return r, err
}

func (j Journal) BindJob(ctx context.Context, intent Intent, uid string) error {
	return j.update(ctx, intent, uid, false)
}

// Complete records evidence already verified by the Move action owner.
func (j Journal) Complete(ctx context.Context, intent Intent, uid string) error {
	return j.update(ctx, intent, uid, true)
}

func (j Journal) update(ctx context.Context, intent Intent, uid string, complete bool) error {
	if uid == "" {
		return fmt.Errorf("cleanup Job UID is required")
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := j.Client.CoreV1().ConfigMaps(j.Namespace).Get(ctx, Name(intent.MoveUID), metav1.GetOptions{})
		if err != nil {
			return err
		}
		r, err := Decode(cm)
		if err != nil {
			return err
		}
		if r.Intent != intent || (r.JobUID != "" && r.JobUID != uid) || (complete && r.JobUID != uid) {
			return fmt.Errorf("cleanup request or Job identity changed")
		}
		if r.JobUID == uid && (!complete || (r.Completed && !r.CompletedAt.IsZero())) {
			return nil
		}
		if cm.Annotations == nil {
			cm.Annotations = map[string]string{}
		}
		cm.Annotations[jobUIDKey] = uid
		if complete {
			j.markComplete(cm)
		}
		_, err = j.Client.CoreV1().ConfigMaps(j.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}
