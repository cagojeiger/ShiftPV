package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
	poolcapacity "github.com/cagojeiger/ShiftPV/src/pool/capacity"
)

const (
	admissionNamespaceLabel = "shiftpv.io/admission"
	placementHoldName       = "shiftpv.io/placement-hold"
	placementAnnotationKey  = "shiftpv.io/placement"
)

type Repository interface {
	ListVolumes(context.Context) (map[string]volumeapi.State, error)
	Get(context.Context, string) (volumeapi.State, error)
	CompareAndSetState(context.Context, string, string, string, string, volumeapi.State) error
	Pools(context.Context) ([]volumeapi.Pool, error)
	ReadyPools(context.Context) ([]volumeapi.Pool, error)
	CreateMove(context.Context, string, volumeapi.MoveSpec) (volumeapi.Move, error)
	DeleteMove(context.Context, string, string) error
	ListMoves(context.Context) ([]volumeapi.Move, error)
	SetMoveStatus(context.Context, string, string, volumeapi.MoveStatus) error
}

type CapacityProbe interface {
	StatFS(context.Context, string) (poolcapacity.Filesystem, error)
	VolumeUsage(context.Context, string, string) (int64, error)
}

type Reconciler struct {
	Client             kubernetes.Interface
	Repository         Repository
	CapacityProbe      CapacityProbe
	PoolLocks          *poolcapacity.Locker
	Namespace          string
	HelperImage        string
	ServiceAccountName string
	Cleanups           *cleanupapi.Store
	CleanupOperator    interface {
		Reclaim(context.Context, cleanupapi.Cleanup, *cleanupapi.Store) (cleanupapi.Cleanup, error)
	}
	Interval         time.Duration
	Now              func() time.Time
	Recorder         record.EventRecorder
	Wake             <-chan struct{}
	ObserveDiscovery func(map[string]int, error)
}

func (r *Reconciler) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	interval := r.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.ReconcileAll(ctx); err != nil && !errors.Is(err, context.Canceled) {
			klog.Errorf("reconcile ShiftPV mobility: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-r.Wake:
		case <-ticker.C:
		}
	}
}

func (r *Reconciler) ReconcileAll(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if err := r.discoverMoves(ctx); err != nil {
		return err
	}
	moves, err := r.Repository.ListMoves(ctx)
	if err != nil {
		return err
	}
	var reconcileErrors []error
	for _, move := range moves {
		phase := fsm.Phase(move.Status.Phase)
		if phase == fsm.PhaseBlocked && move.Spec.Recovery == "ResumeOwner" && move.Status.RecoveryPhase != recoveryRecovered {
			if err := r.reconcileRecovery(ctx, move); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("recover move %s: %w", move.Name, err))
			}
			continue
		}
		if phase == fsm.PhaseSucceeded || phase == fsm.PhaseBlocked {
			continue
		}
		if err := r.reconcileMove(ctx, move); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("move %s: %w", move.Name, err))
		}
	}
	return errors.Join(reconcileErrors...)
}

func (r *Reconciler) validate() error {
	if r == nil || r.Client == nil || r.Repository == nil || r.Namespace == "" || r.HelperImage == "" {
		return fmt.Errorf("mobility reconciler is not configured")
	}
	return nil
}
