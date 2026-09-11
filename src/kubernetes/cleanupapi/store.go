package cleanupapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/cagojeiger/ShiftPV/src/volume"
)

var Resource = schema.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvcleanups"}

var ErrConflict = errors.New("ShiftPVCleanup state precondition failed")

const VolumeIDLabel = "shiftpv.io/volume-id"

const (
	PhasePending     = "Pending"
	PhaseRunning     = "Running"
	PhaseVerifying   = "Verifying"
	PhaseCompleted   = "Completed"
	PhaseNeedsReview = "NeedsReview"
)

type CopyIdentity = volume.CopyIdentity

type Authority struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

type Spec struct {
	OperationID    string       `json:"operationID"`
	Target         CopyIdentity `json:"target"`
	Reason         string       `json:"reason"`
	Authority      Authority    `json:"authority"`
	ReservationUID string       `json:"reservationUID,omitempty"`
	Approved       bool         `json:"approved"`
}

type Executor struct {
	JobName  string `json:"jobName"`
	JobUID   string `json:"jobUID"`
	PodUID   string `json:"podUID,omitempty"`
	NodeName string `json:"nodeName"`
}

type Receipt struct {
	OperationID        string `json:"operationID"`
	ExecutorUID        string `json:"executorUID"`
	ObservedAt         string `json:"observedAt"`
	Retired            bool   `json:"retired"`
	Purged             bool   `json:"purged"`
	LocalReceiptDigest string `json:"localReceiptDigest,omitempty"`
}

type Status struct {
	Phase              string    `json:"phase,omitempty"`
	ObservedGeneration int64     `json:"observedGeneration,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime string    `json:"lastTransitionTime,omitempty"`
	Executor           *Executor `json:"executor,omitempty"`
	Receipt            *Receipt  `json:"receipt,omitempty"`
	SettledAt          string    `json:"settledAt,omitempty"`
}

type Cleanup struct {
	Name            string
	UID             string
	ResourceVersion string
	Generation      int64
	Spec            Spec
	Status          Status
}

type Store struct {
	Client dynamic.Interface
	Now    func() time.Time
}

func Name(target CopyIdentity) string {
	encoded, _ := json.Marshal(target)
	sum := sha256.Sum256(encoded)
	return "shiftpv-cleanup-" + hex.EncodeToString(sum[:16])
}

func (s Spec) Validate() error {
	if !volume.ValidIdentityToken(s.OperationID) || s.Target.Validate() != nil ||
		!volume.ValidIdentityToken(s.Authority.UID) || !volume.ValidObjectName(s.Authority.Name) ||
		(s.ReservationUID != "" && !volume.ValidIdentityToken(s.ReservationUID)) {
		return fmt.Errorf("invalid cleanup identity")
	}
	if s.Reason != "MoveSource" && s.Reason != "VolumeDelete" && s.Reason != "OrphanReclaim" {
		return fmt.Errorf("invalid cleanup reason %q", s.Reason)
	}
	if s.Authority.Kind != "Namespace" && s.Authority.Kind != "ShiftPVVolume" && s.Authority.Kind != "ShiftPVMove" {
		return fmt.Errorf("invalid cleanup authority %q", s.Authority.Kind)
	}
	switch s.Reason {
	case "VolumeDelete":
		if !s.Approved || s.Authority.Kind != "ShiftPVVolume" || s.Authority.Name != s.Target.VolumeID || s.Authority.UID != s.Target.VolumeUID {
			return fmt.Errorf("volume cleanup authority does not match the target")
		}
	case "MoveSource":
		if !s.Approved || s.Authority.Kind != "ShiftPVMove" {
			return fmt.Errorf("move cleanup requires approved ShiftPVMove authority")
		}
	case "OrphanReclaim":
		if s.Authority.Kind != "Namespace" || s.Authority.Name != "kube-system" || s.Authority.UID != s.Target.InstallationID {
			return fmt.Errorf("orphan cleanup authority does not match the installation")
		}
	}
	return nil
}

func (s *Store) Ensure(ctx context.Context, spec Spec) (Cleanup, error) {
	if err := s.validate(); err != nil {
		return Cleanup{}, err
	}
	if err := spec.Validate(); err != nil {
		return Cleanup{}, err
	}
	name := Name(spec.Target)
	resource := s.Client.Resource(Resource)
	object, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		encoded, encodeErr := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
		if encodeErr != nil {
			return Cleanup{}, encodeErr
		}
		object, err = resource.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "shiftpv.io/v1alpha1",
			"kind":       "ShiftPVCleanup",
			"metadata":   map[string]any{"name": name, "labels": map[string]any{VolumeIDLabel: spec.Target.VolumeID}},
			"spec":       encoded,
		}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			object, err = resource.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return Cleanup{}, fmt.Errorf("ensure ShiftPVCleanup: %w", err)
	}
	cleanup, err := fromUnstructured(object)
	if err != nil {
		return Cleanup{}, err
	}
	if !reflect.DeepEqual(cleanup.Spec, spec) {
		return Cleanup{}, fmt.Errorf("%w: operation ID is bound to another intent", ErrConflict)
	}
	return cleanup, nil
}

func (s *Store) Get(ctx context.Context, name string) (Cleanup, error) {
	if err := s.validate(); err != nil {
		return Cleanup{}, err
	}
	object, err := s.Client.Resource(Resource).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Cleanup{}, err
	}
	return fromUnstructured(object)
}

func (s *Store) List(ctx context.Context) ([]Cleanup, error) {
	return s.list(ctx, metav1.ListOptions{})
}

func (s *Store) ListForVolume(ctx context.Context, volumeID string) ([]Cleanup, error) {
	if err := volume.ValidateID(volumeID); err != nil {
		return nil, err
	}
	selector := labels.Set{VolumeIDLabel: volumeID}.AsSelector().String()
	return s.list(ctx, metav1.ListOptions{LabelSelector: selector})
}

func (s *Store) list(ctx context.Context, options metav1.ListOptions) ([]Cleanup, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	objects, err := s.Client.Resource(Resource).List(ctx, options)
	if err != nil {
		return nil, err
	}
	result := make([]Cleanup, 0, len(objects.Items))
	for index := range objects.Items {
		cleanup, decodeErr := fromUnstructured(&objects.Items[index])
		if decodeErr != nil {
			return nil, decodeErr
		}
		result = append(result, cleanup)
	}
	return result, nil
}

func (s *Store) UpdateStatus(ctx context.Context, name, uid string, next Status) error {
	if err := s.validate(); err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource := s.Client.Resource(Resource)
		object, err := resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		current, err := fromUnstructured(object)
		if err != nil {
			return err
		}
		if uid == "" || current.UID != uid || object.GetDeletionTimestamp() != nil {
			return ErrConflict
		}
		if next.ObservedGeneration == 0 {
			next.ObservedGeneration = current.Generation
		}
		if next.LastTransitionTime == "" {
			now := time.Now().UTC()
			if s.Now != nil {
				now = s.Now().UTC()
			}
			next.LastTransitionTime = now.Format(time.RFC3339Nano)
		}
		if err := validateTransition(current, next); err != nil {
			return err
		}
		encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&next)
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(object.Object, encoded, "status"); err != nil {
			return err
		}
		_, err = resource.UpdateStatus(ctx, object, metav1.UpdateOptions{})
		return err
	})
}

func validateTransition(current Cleanup, next Status) error {
	phase := current.Status.Phase
	if phase == "" {
		phase = PhasePending
	}
	allowed := map[string]map[string]bool{
		PhasePending:     {PhasePending: true, PhaseRunning: true, PhaseNeedsReview: true},
		PhaseRunning:     {PhaseRunning: true, PhaseVerifying: true, PhaseNeedsReview: true},
		PhaseVerifying:   {PhaseVerifying: true, PhaseCompleted: true, PhaseNeedsReview: true},
		PhaseCompleted:   {PhaseCompleted: true, PhaseNeedsReview: true},
		PhaseNeedsReview: {PhasePending: true, PhaseNeedsReview: true},
	}
	if !allowed[phase][next.Phase] {
		return fmt.Errorf("%w: phase %s cannot transition to %s", ErrConflict, phase, next.Phase)
	}
	if phase == PhaseCompleted && next.Phase == PhaseNeedsReview &&
		(next.Reason != "CopyReappeared" || !reflect.DeepEqual(current.Status.Executor, next.Executor) ||
			!reflect.DeepEqual(current.Status.Receipt, next.Receipt) || current.Status.SettledAt == "" || current.Status.SettledAt != next.SettledAt) {
		return fmt.Errorf("%w: only an observed exact-copy reappearance may reopen a completed cleanup", ErrConflict)
	}
	if phase == PhaseNeedsReview && next.Phase == PhasePending &&
		(!current.Spec.Approved || current.Spec.Reason != "OrphanReclaim" || current.Status.Executor != nil || current.Status.Receipt != nil || next.Executor != nil || next.Receipt != nil) {
		return fmt.Errorf("%w: only an unexecuted approved orphan may be re-evaluated", ErrConflict)
	}
	if current.Status.Executor != nil && !reflect.DeepEqual(current.Status.Executor, next.Executor) {
		return fmt.Errorf("%w: executor identity is immutable", ErrConflict)
	}
	if next.Phase == PhaseRunning && (next.Executor == nil || !current.Spec.Approved) {
		return fmt.Errorf("%w: Running requires an approved intent and executor", ErrConflict)
	}
	if current.Status.Receipt != nil && !reflect.DeepEqual(current.Status.Receipt, next.Receipt) {
		return fmt.Errorf("%w: receipt is immutable", ErrConflict)
	}
	if next.Receipt != nil {
		if next.Executor == nil || next.Receipt.OperationID != current.Spec.OperationID || next.Receipt.ExecutorUID != next.Executor.JobUID {
			return fmt.Errorf("%w: receipt identity does not match intent and executor", ErrConflict)
		}
		if _, err := time.Parse(time.RFC3339Nano, next.Receipt.ObservedAt); err != nil {
			return fmt.Errorf("%w: invalid receipt time", ErrConflict)
		}
	}
	if next.Phase == PhaseVerifying && next.Receipt == nil {
		return fmt.Errorf("%w: Verifying requires a receipt", ErrConflict)
	}
	if next.Phase == PhaseCompleted {
		if next.Receipt == nil || !next.Receipt.Purged || next.SettledAt == "" {
			return fmt.Errorf("%w: Completed requires a purged receipt and settlement time", ErrConflict)
		}
		if _, err := time.Parse(time.RFC3339Nano, next.SettledAt); err != nil {
			return fmt.Errorf("%w: invalid settlement time", ErrConflict)
		}
	}
	return nil
}

func fromUnstructured(object *unstructured.Unstructured) (Cleanup, error) {
	var spec Spec
	value, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		return Cleanup{}, fmt.Errorf("decode ShiftPVCleanup %q spec", object.GetName())
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(value, &spec); err != nil {
		return Cleanup{}, err
	}
	if err := spec.Validate(); err != nil {
		return Cleanup{}, err
	}
	var status Status
	if value, found, err = unstructured.NestedMap(object.Object, "status"); err != nil {
		return Cleanup{}, err
	} else if found {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(value, &status); err != nil {
			return Cleanup{}, err
		}
	}
	return Cleanup{Name: object.GetName(), UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion(), Generation: object.GetGeneration(), Spec: spec, Status: status}, nil
}

func (s *Store) validate() error {
	if s == nil || s.Client == nil {
		return fmt.Errorf("Kubernetes dynamic client is required")
	}
	return nil
}
