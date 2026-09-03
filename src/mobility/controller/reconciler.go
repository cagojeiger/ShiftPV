package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
)

const (
	admissionNamespaceLabel = "shiftpv.io/admission"
	placementHoldName       = "shiftpv.io/placement-hold"
)

type Repository interface {
	ListVolumes(context.Context) (map[string]volumeapi.State, error)
	Get(context.Context, string) (volumeapi.State, error)
	CompareAndSetState(context.Context, string, string, string, string, volumeapi.State) error
	Pools(context.Context) ([]volumeapi.Pool, error)
	CreateMove(context.Context, string, volumeapi.MoveSpec) (volumeapi.Move, error)
	ListMoves(context.Context) ([]volumeapi.Move, error)
	SetMoveStatus(context.Context, string, volumeapi.MoveStatus) error
}

type Reconciler struct {
	Client      kubernetes.Interface
	Repository  Repository
	Namespace   string
	HelperImage string
	Interval    time.Duration
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
		if phase == fsm.PhaseSucceeded || phase == fsm.PhaseBlocked {
			continue
		}
		if err := r.reconcileMove(ctx, move); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("move %s: %w", move.Name, err))
		}
	}
	return errors.Join(reconcileErrors...)
}

func (r *Reconciler) discoverMoves(ctx context.Context) error {
	volumes, err := r.Repository.ListVolumes(ctx)
	if err != nil {
		return err
	}
	moves, err := r.Repository.ListMoves(ctx)
	if err != nil {
		return err
	}
	activeByVolume := make(map[string]bool)
	for _, move := range moves {
		state, exists := volumes[move.Spec.VolumeID]
		if move.Status.Phase != string(fsm.PhaseSucceeded) &&
			(move.Status.Phase != string(fsm.PhaseBlocked) || (exists && state.OwnerNode == move.Spec.SourceNode)) {
			activeByVolume[move.Spec.VolumeID] = true
		}
	}
	for volumeID, state := range volumes {
		if state.Phase != volumeapi.PhaseReady || state.ActiveMove != "" || activeByVolume[volumeID] {
			continue
		}
		node, err := r.Client.CoreV1().Nodes().Get(ctx, state.OwnerNode, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("read owner Node %q: %w", state.OwnerNode, err)
		}
		if !node.Spec.Unschedulable || !nodeReady(node) {
			continue
		}
		candidate := volumeapi.Move{Spec: volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: state.OwnerNode}}
		observed, err := r.observe(ctx, candidate)
		if err != nil {
			return fmt.Errorf("preflight volume %s: %w", volumeID, err)
		}
		if !observed.FSM.PreconditionsValid {
			continue
		}
		move, err := r.Repository.CreateMove(ctx, moveGenerateName(volumeID), volumeapi.MoveSpec{VolumeID: volumeID, SourceNode: state.OwnerNode})
		if err != nil {
			return err
		}
		move.Status.Phase = string(fsm.PhasePending)
		if err := r.Repository.SetMoveStatus(ctx, move.Name, move.Status); err != nil {
			return err
		}
		activeByVolume[volumeID] = true
		klog.Infof("created ShiftPVMove %s for cordoned node %s volume %s", move.Name, state.OwnerNode, volumeID)
	}
	return nil
}

func (r *Reconciler) reconcileMove(ctx context.Context, move volumeapi.Move) error {
	if move.Status.Phase == "" {
		move.Status.Phase = string(fsm.PhasePending)
		return r.Repository.SetMoveStatus(ctx, move.Name, move.Status)
	}
	observed, err := r.observe(ctx, move)
	if err != nil {
		return err
	}
	decision, err := fsm.Decide(fsm.Phase(move.Status.Phase), observed.FSM)
	if err != nil {
		return err
	}
	if err := r.execute(ctx, &move, observed, decision); err != nil {
		return err
	}
	if move.Status.Phase != string(decision.Next) || decision.Reason != "" || decision.Action != fsm.ActionWait {
		move.Status.Phase = string(decision.Next)
		move.Status.Reason = decision.Reason
		if decision.Next != fsm.PhaseBlocked {
			move.Status.Message = ""
		}
		if err := r.Repository.SetMoveStatus(ctx, move.Name, move.Status); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) validate() error {
	if r == nil || r.Client == nil || r.Repository == nil || r.Namespace == "" || r.HelperImage == "" {
		return fmt.Errorf("mobility reconciler is not configured")
	}
	return nil
}

func moveGenerateName(volumeID string) string {
	return "move-" + strings.TrimPrefix(volumeID, "shiftpv-") + "-"
}
