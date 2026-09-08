package metrics

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/pool/capacity"
)

type Inventory interface {
	ListPools(context.Context) ([]volumeapi.Pool, error)
	ListVolumes(context.Context) (map[string]volumeapi.State, error)
	ListMoves(context.Context) ([]volumeapi.Move, error)
}

type Controller struct {
	Exporter   *Exporter
	Inventory  Inventory
	Client     kubernetes.Interface
	Namespace  string
	Interval   time.Duration
	StaleAfter time.Duration
}

func (c *Controller) Run(ctx context.Context) {
	if c.Interval <= 0 {
		klog.Error("metrics snapshot interval must be positive")
		return
	}
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		snapshotCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := c.Refresh(snapshotCtx); err != nil {
			klog.V(2).Infof("metrics snapshot failed: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Controller) Refresh(ctx context.Context) (refreshErr error) {
	defer func() {
		if refreshErr != nil {
			c.Exporter.Cache.update("metadata", nil, false)
		}
	}()
	pools, err := c.Inventory.ListPools(ctx)
	if err != nil {
		return err
	}
	volumes, err := c.Inventory.ListVolumes(ctx)
	if err != nil {
		return err
	}
	moves, err := c.Inventory.ListMoves(ctx)
	if err != nil {
		return err
	}
	reservations, err := c.Client.CoreV1().ConfigMaps(c.Namespace).List(ctx, metav1.ListOptions{LabelSelector: capacity.ReservationSelector})
	if err != nil {
		return err
	}
	staleAfter := c.StaleAfter
	if staleAfter <= 0 {
		staleAfter = volumeapi.DefaultPoolReadinessStaleAfter
	}
	values := c.poolSamples(pools, volumes, moves, reservations.Items, staleAfter)
	counts := make(map[string]int)
	for _, state := range volumes {
		counts[bounded(state.Phase, volumePhases)]++
	}
	for _, phase := range volumePhases {
		values = append(values, sample{"volumes", float64(counts[phase]), []string{phase}})
	}
	active := make(map[string]volumeapi.Move, len(moves))
	for _, move := range moves {
		active[move.Name] = move
	}
	counts = make(map[string]int)
	for id, state := range volumes {
		if state.ActiveMove == "" {
			continue
		}
		move, exists := active[state.ActiveMove]
		if !exists || move.Spec.VolumeID != id {
			return fmt.Errorf("active Move link is incomplete")
		}
		counts[bounded(move.Status.Phase, movePhases)]++
	}
	for _, phase := range movePhases {
		values = append(values, sample{"moves", float64(counts[phase]), []string{phase}})
	}
	c.Exporter.Cache.update("metadata", values, true)
	return nil
}

func (c *Controller) poolSamples(pools []volumeapi.Pool, volumes map[string]volumeapi.State, moves []volumeapi.Move, reservations []corev1.ConfigMap, staleAfter time.Duration) []sample {
	var values []sample
	for _, pool := range pools {
		labels := []string{pool.Name, pool.NodeName}
		ready, _ := pool.ReadyAt(time.Now(), staleAfter)
		values = append(values, sample{"pool_ready", boolValue(ready), labels})
		q, err := resource.ParseQuantity(pool.CapacityLimit)
		limit, exact := q.AsInt64()
		reserved, accountingErr := capacity.ReservedBytes(reservations, volumes, moves, pool.NodeName)
		valid := err == nil && exact && limit > 0 && accountingErr == nil
		values = append(values, sample{"pool_accounting_valid", boolValue(valid), labels})
		if !valid {
			// Keep only this Pool's last good numbers; validity explicitly marks them stale.
			c.Exporter.Cache.mu.RLock()
			for _, previous := range c.Exporter.Cache.groups["metadata"].samples {
				if (previous.name == "pool_capacity_limit_bytes" || previous.name == "pool_reserved_bytes" || previous.name == "pool_unregistered_reserved_bytes") && previous.labels[0] == pool.Name && previous.labels[1] == pool.NodeName {
					values = append(values, previous)
				}
			}
			c.Exporter.Cache.mu.RUnlock()
			continue
		}
		var unregistered int64
		for _, reservation := range reservations {
			if _, exists := volumes[reservation.Name]; !exists && reservation.Data["nodeName"] == pool.NodeName {
				n, _ := strconv.ParseInt(reservation.Data["capacity"], 10, 64)
				unregistered += n // A validated subset of reserved, so it cannot overflow.
			}
		}
		values = append(values, sample{"pool_capacity_limit_bytes", float64(limit), labels}, sample{"pool_reserved_bytes", float64(reserved), labels}, sample{"pool_unregistered_reserved_bytes", float64(unregistered), labels})
	}
	return values
}
