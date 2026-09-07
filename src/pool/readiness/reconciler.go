package readiness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

type Repository interface {
	PoolForNode(context.Context, string) (volumeapi.Pool, error)
	SetPoolStatus(context.Context, string, string, volumeapi.PoolStatus) error
}

type Reconciler struct {
	NodeName  string
	Pools     Repository
	Inspector Inspector
	Interval  time.Duration
	Now       func() time.Time
}

func (r *Reconciler) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if err := r.reconcileAndLog(ctx); err != nil && errors.Is(err, context.Canceled) {
		return nil
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.reconcileAndLog(ctx); err != nil && errors.Is(err, context.Canceled) {
				return nil
			}
		}
	}
}

func (r *Reconciler) Reconcile(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	pool, err := r.Pools.PoolForNode(ctx, r.NodeName)
	if errors.Is(err, volumeapi.ErrPoolNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	result := r.Inspector.Inspect(pool)
	status := pool.Status
	status.ObservedGeneration = pool.Generation
	status.LastProbeTime = metav1.NewTime(now)
	meta.RemoveStatusCondition(&status.Conditions, volumeapi.PoolConditionMounted)
	for _, condition := range conditions(result, pool.Generation, now) {
		meta.SetStatusCondition(&status.Conditions, condition)
	}
	return r.Pools.SetPoolStatus(ctx, pool.Name, r.NodeName, status)
}

func (r *Reconciler) reconcileAndLog(ctx context.Context) error {
	err := r.Reconcile(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		klog.Errorf("reconcile ShiftPVPool readiness on node %s: %v", r.NodeName, err)
	}
	return err
}

func (r *Reconciler) validate() error {
	if r.NodeName == "" || r.Pools == nil || r.Inspector == nil || r.Interval <= 0 {
		return fmt.Errorf("Pool readiness reconciler configuration is incomplete")
	}
	return nil
}

func conditions(result Result, generation int64, now time.Time) []metav1.Condition {
	accessible := condition(volumeapi.PoolConditionAccessible, result.Accessible, generation, now)
	writable := condition(volumeapi.PoolConditionWritable, result.Writable, generation, now)
	capacity := condition(volumeapi.PoolConditionCapacityReadable, result.CapacityReadable, generation, now)
	readyCheck := Check{OK: true, Known: true, Reason: "PoolReady", Message: "Pool directory is accessible, writable, and capacity-readable"}
	for _, candidate := range []Check{result.Accessible, result.Writable, result.CapacityReadable} {
		if !candidate.Known || !candidate.OK {
			readyCheck.OK = false
			readyCheck.Reason = candidate.Reason
			readyCheck.Message = candidate.Message
			break
		}
	}
	return []metav1.Condition{accessible, writable, capacity, condition(volumeapi.PoolConditionReady, readyCheck, generation, now)}
}

func condition(conditionType string, check Check, generation int64, now time.Time) metav1.Condition {
	status := metav1.ConditionUnknown
	if check.Known {
		status = metav1.ConditionFalse
		if check.OK {
			status = metav1.ConditionTrue
		}
	}
	return metav1.Condition{
		Type: conditionType, Status: status, ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(now), Reason: check.Reason, Message: check.Message,
	}
}
