package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

const capacityReservationSelector = "app.kubernetes.io/name=shiftpv,app.kubernetes.io/component=volume-reservation"

func (r *Reconciler) ensureCapacity(ctx context.Context, move *volumeapi.Move, observed observation) error {
	if observed.DestinationNode == "" || r.CapacityProbe == nil || r.PoolLocks == nil {
		return fmt.Errorf("destination capacity admission is not configured")
	}
	unlock := r.PoolLocks.Lock(observed.DestinationNode)
	defer unlock()

	requested, logicalReserved, physicalPending, limit, err := r.destinationCapacity(ctx, *move, observed.DestinationNode)
	if err != nil {
		return err
	}
	sourceBytes := move.Status.SourceBytes
	if sourceBytes <= 0 {
		sourceBytes, err = r.CapacityProbe.VolumeUsage(ctx, move.Spec.SourceNode, move.Spec.VolumeID)
		if err != nil {
			return fmt.Errorf("measure source volume usage: %w", err)
		}
	}
	stats, err := r.CapacityProbe.StatFS(ctx, observed.DestinationNode)
	if err != nil {
		return fmt.Errorf("inspect destination Pool filesystem: %w", err)
	}

	previous := move.Status
	move.Status.DestinationNode = observed.DestinationNode
	move.Status.SourceBytes = sourceBytes
	move.Status.CapacityApproved = false
	move.Status.CapacityReason = ""
	if requested > limit-logicalReserved {
		move.Status.CapacityReason = "DestinationReservationLimit"
	} else if physicalPending > stats.AvailableBytes || sourceBytes > stats.AvailableBytes-physicalPending {
		move.Status.CapacityReason = "DestinationFilesystemSpace"
	} else {
		move.Status.CapacityApproved = true
	}
	return r.persistMoveStatus(ctx, move, previous)
}

func (r *Reconciler) destinationCapacity(ctx context.Context, current volumeapi.Move, destination string) (requested, logicalReserved, physicalPending, limit int64, err error) {
	pool, err := r.poolForNode(ctx, destination)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	quantity, err := resource.ParseQuantity(pool.CapacityLimit)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("parse destination Pool capacity limit: %w", err)
	}
	limit, exact := quantity.AsInt64()
	if !exact || limit <= 0 {
		return 0, 0, 0, 0, fmt.Errorf("destination Pool capacity limit must be positive bytes")
	}

	reservations, err := r.Client.CoreV1().ConfigMaps(r.Namespace).List(ctx, metav1.ListOptions{LabelSelector: capacityReservationSelector})
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("list volume reservations: %w", err)
	}
	volumes, err := r.Repository.ListVolumes(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	moves, err := r.Repository.ListMoves(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	capacities := make(map[string]int64, len(reservations.Items))
	for index := range reservations.Items {
		reservation := &reservations.Items[index]
		bytes, parseErr := strconv.ParseInt(reservation.Data["capacity"], 10, 64)
		if reservation.Name != reservation.Data["volumeID"] || parseErr != nil || bytes <= 0 {
			return 0, 0, 0, 0, fmt.Errorf("reservation %q is invalid", reservation.Name)
		}
		capacities[reservation.Name] = bytes
		owner := reservation.Data["nodeName"]
		if state, exists := volumes[reservation.Name]; exists {
			owner = state.OwnerNode
		}
		if owner == destination {
			if logicalReserved > math.MaxInt64-bytes {
				return 0, 0, 0, 0, fmt.Errorf("destination reservation total overflows int64")
			}
			logicalReserved += bytes
		}
	}
	requested, exists := capacities[current.Spec.VolumeID]
	if !exists {
		return 0, 0, 0, 0, fmt.Errorf("volume %q has no capacity reservation", current.Spec.VolumeID)
	}
	for _, move := range moves {
		if move.Name == current.Name || !move.Status.CapacityApproved || move.Status.DestinationNode != destination {
			continue
		}
		state, exists := volumes[move.Spec.VolumeID]
		if !exists {
			return 0, 0, 0, 0, fmt.Errorf("approved move %q has no volume state", move.Name)
		}
		if !volumeapi.MoveReservesDestination(move, state, destination) {
			continue
		}
		bytes, exists := capacities[move.Spec.VolumeID]
		if !exists || logicalReserved > math.MaxInt64-bytes || physicalPending > math.MaxInt64-move.Status.SourceBytes {
			return 0, 0, 0, 0, fmt.Errorf("approved move %q has invalid capacity state", move.Name)
		}
		logicalReserved += bytes
		physicalPending += move.Status.SourceBytes
	}
	if logicalReserved > limit {
		return requested, logicalReserved, physicalPending, limit, nil
	}
	return requested, logicalReserved, physicalPending, limit, nil
}

func (r *Reconciler) poolForNode(ctx context.Context, nodeName string) (volumeapi.Pool, error) {
	pools, err := r.Repository.ReadyPools(ctx)
	if err != nil {
		return volumeapi.Pool{}, err
	}
	for _, pool := range pools {
		if pool.NodeName == nodeName {
			return pool, nil
		}
	}
	return volumeapi.Pool{}, fmt.Errorf("node %q has no Ready Pool", nodeName)
}
