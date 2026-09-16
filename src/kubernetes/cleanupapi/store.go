package cleanupapi

import (
	"context"
	"fmt"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
	"github.com/project-jelly/ShiftPV/src/volume"
)

type Store struct {
	Client dynamic.Interface
	Now    func() time.Time
}

func (s *Store) Ensure(ctx context.Context, spec Spec) (Cleanup, error) {
	if err := s.validate(); err != nil {
		return Cleanup{}, err
	}
	if err := spec.Validate(); err != nil {
		return Cleanup{}, err
	}
	var result Cleanup
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource, finalizer, err := s.parentResource(spec.Authority)
		if err != nil {
			return err
		}
		object, err := resource.Get(ctx, spec.Authority.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := validateParent(object, spec.Authority, finalizer); err != nil {
			return err
		}
		current, found, err := journalFromParent(object)
		if err != nil {
			return err
		}
		if found {
			if !reflect.DeepEqual(current.Spec, spec) {
				return fmt.Errorf("%w: parent cleanup slot is bound to another intent", ErrConflict)
			}
			result = current
			return nil
		}
		status := Status{
			Phase:              PhasePending,
			ObservedGeneration: object.GetGeneration(),
			LastTransitionTime: s.now().Format(time.RFC3339Nano),
		}
		if err := setJournal(object, journal{Spec: spec, Status: status}); err != nil {
			return err
		}
		updated, err := resource.UpdateStatus(ctx, object, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		result, _, err = journalFromParent(updated)
		return err
	})
	if err != nil {
		return Cleanup{}, fmt.Errorf("ensure parent cleanup journal: %w", err)
	}
	return result, nil
}

// Get reads status.cleanup from the exact parent identity. A terminating
// parent remains usable while its ShiftPV protection finalizer is present.
func (s *Store) Get(ctx context.Context, authority Authority) (Cleanup, error) {
	if err := s.validate(); err != nil {
		return Cleanup{}, err
	}
	resource, finalizer, err := s.parentResource(authority)
	if err != nil {
		return Cleanup{}, err
	}
	object, err := resource.Get(ctx, authority.Name, metav1.GetOptions{})
	if err != nil {
		return Cleanup{}, err
	}
	if err := validateParent(object, authority, finalizer); err != nil {
		return Cleanup{}, err
	}
	cleanup, found, err := journalFromParent(object)
	if err != nil {
		return Cleanup{}, err
	}
	if !found {
		return Cleanup{}, fmt.Errorf("%w: %s %q", ErrNoJournal, authority.Kind, authority.Name)
	}
	return cleanup, nil
}

func (s *Store) List(ctx context.Context) ([]Cleanup, error) {
	return s.list(ctx, "")
}

func (s *Store) ListForVolume(ctx context.Context, volumeID string) ([]Cleanup, error) {
	if err := volume.ValidateID(volumeID); err != nil {
		return nil, err
	}
	return s.list(ctx, volumeID)
}

func (s *Store) list(ctx context.Context, volumeID string) ([]Cleanup, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	result := make([]Cleanup, 0)
	for _, resource := range []schema.GroupVersionResource{volumeapi.VolumeResource, volumeapi.MoveResource} {
		objects, err := s.Client.Resource(resource).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for index := range objects.Items {
			cleanup, found, decodeErr := journalFromParent(&objects.Items[index])
			if decodeErr != nil {
				return nil, decodeErr
			}
			if found && (volumeID == "" || cleanup.Spec.Target.VolumeID == volumeID) {
				result = append(result, cleanup)
			}
		}
	}
	return result, nil
}

func (s *Store) UpdateStatus(ctx context.Context, expected Cleanup, next Status) error {
	if err := s.validate(); err != nil {
		return err
	}
	if expected.UID == "" || expected.Name != cleanupName(expected.Spec.Target) || expected.UID != expected.Spec.Authority.UID || expected.Spec.Validate() != nil {
		return ErrConflict
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resource, finalizer, err := s.parentResource(expected.Spec.Authority)
		if err != nil {
			return err
		}
		object, err := resource.Get(ctx, expected.Spec.Authority.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := validateParent(object, expected.Spec.Authority, finalizer); err != nil {
			return err
		}
		current, found, err := journalFromParent(object)
		if err != nil {
			return err
		}
		if !found || current.UID != expected.UID || current.Name != expected.Name || !reflect.DeepEqual(current.Spec, expected.Spec) {
			return ErrConflict
		}
		if next.ObservedGeneration == 0 {
			next.ObservedGeneration = object.GetGeneration()
		}
		if next.LastTransitionTime == "" {
			if next.Phase == current.Status.Phase && current.Status.LastTransitionTime != "" {
				next.LastTransitionTime = current.Status.LastTransitionTime
			} else {
				next.LastTransitionTime = s.now().Format(time.RFC3339Nano)
			}
		}
		if reflect.DeepEqual(current.Status, next) {
			return nil
		}
		if err := validateTransition(current, next); err != nil {
			return err
		}
		if err := setJournal(object, journal{Spec: current.Spec, Status: next}); err != nil {
			return err
		}
		_, err = resource.UpdateStatus(ctx, object, metav1.UpdateOptions{})
		return err
	})
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) validate() error {
	if s == nil || s.Client == nil {
		return fmt.Errorf("Kubernetes dynamic client is required")
	}
	return nil
}
