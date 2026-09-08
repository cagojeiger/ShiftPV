package controller

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	poolcapacity "github.com/cagojeiger/ShiftPV/src/pool/capacity"
)

const reservationSelector = poolcapacity.ReservationSelector

type PoolCapacityRegistry interface {
	ReadyPoolForNode(context.Context, string) (volumeapi.Pool, error)
	ListVolumes(context.Context) (map[string]volumeapi.State, error)
	ListMoves(context.Context) ([]volumeapi.Move, error)
}

type PoolCapacityProbe interface {
	StatFS(context.Context, string) (poolcapacity.Filesystem, error)
}

func (s *Service) reserveWithinPool(ctx context.Context, id, requestName, nodeName string, requestedBytes int64, data map[string]string) error {
	existing, err := s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, id, metav1.GetOptions{})
	if err == nil {
		return validateReservation(existing, requestName, data)
	}
	if !apierrors.IsNotFound(err) {
		return kubernetesAPIError("read volume reservation", err)
	}

	unlock := s.poolLifecycles.lock(nodeName)
	if s.PoolLocks != nil {
		unlock()
		unlock = s.PoolLocks.Lock(nodeName)
	}
	defer unlock()

	existing, err = s.Client.CoreV1().ConfigMaps(s.Namespace).Get(ctx, id, metav1.GetOptions{})
	if err == nil {
		return validateReservation(existing, requestName, data)
	}
	if !apierrors.IsNotFound(err) {
		return kubernetesAPIError("read volume reservation", err)
	}

	pool, err := s.CapacityPools.ReadyPoolForNode(ctx, nodeName)
	if err != nil {
		if errors.Is(err, volumeapi.ErrPoolConfiguration) || errors.Is(err, volumeapi.ErrPoolNotFound) || errors.Is(err, volumeapi.ErrPoolNotReady) {
			return status.Errorf(codes.FailedPrecondition, "read selected Pool: %v", err)
		}
		return kubernetesAPIError("read selected Pool", err)
	}
	limitBytes, err := poolLimitBytes(pool)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "Pool %q capacity limit is invalid: %v", pool.Name, err)
	}
	reservedBytes, err := s.poolReservedBytes(ctx, nodeName)
	if err != nil {
		return err
	}
	logicalFree := int64(0)
	if reservedBytes < limitBytes {
		logicalFree = limitBytes - reservedBytes
	}
	if requestedBytes > logicalFree {
		return status.Errorf(codes.ResourceExhausted,
			"Pool %q reservation limit exceeded: requested=%d reserved=%d limit=%d",
			pool.Name, requestedBytes, reservedBytes, limitBytes)
	}

	stats, err := s.CapacityProbe.StatFS(ctx, nodeName)
	if err != nil {
		return capacityProbeError("inspect Pool filesystem capacity", err)
	}
	if requestedBytes > stats.AvailableBytes {
		return status.Errorf(codes.ResourceExhausted,
			"Pool %q filesystem space is insufficient: requested=%d available=%d",
			pool.Name, requestedBytes, stats.AvailableBytes)
	}
	return s.createReservation(ctx, id, requestName, data)
}

func (s *Service) poolReservedBytes(ctx context.Context, nodeName string) (int64, error) {
	reservations, err := s.Client.CoreV1().ConfigMaps(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: reservationSelector})
	if err != nil {
		return 0, kubernetesAPIError("list volume reservations", err)
	}
	volumes, err := s.CapacityPools.ListVolumes(ctx)
	if err != nil {
		return 0, kubernetesAPIError("list volume owners", err)
	}
	moves, err := s.CapacityPools.ListMoves(ctx)
	if err != nil {
		return 0, kubernetesAPIError("list capacity-approved moves", err)
	}

	total, err := poolcapacity.ReservedBytes(reservations.Items, volumes, moves, nodeName)
	if err != nil {
		return 0, status.Error(codes.FailedPrecondition, err.Error())
	}
	return total, nil
}

func poolLimitBytes(pool volumeapi.Pool) (int64, error) {
	if pool.CapacityLimit == "" {
		return 0, fmt.Errorf("spec.capacity.limit is required")
	}
	quantity, err := resource.ParseQuantity(pool.CapacityLimit)
	if err != nil {
		return 0, err
	}
	value, exact := quantity.AsInt64()
	if !exact {
		return 0, fmt.Errorf("value cannot be represented as bytes")
	}
	if value <= 0 {
		return 0, fmt.Errorf("value must be greater than zero")
	}
	return value, nil
}

func validateReservation(existing *corev1.ConfigMap, requestName string, data map[string]string) error {
	for key, value := range data {
		if existing.Data[key] != value {
			return status.Errorf(codes.AlreadyExists, "volume %q already exists with incompatible %s", requestName, key)
		}
	}
	return nil
}

func capacityProbeError(operation string, err error) error {
	code := codes.Internal
	var retryable interface{ Retryable() bool }
	switch {
	case errors.Is(err, context.Canceled):
		code = codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	case errors.As(err, &retryable) && retryable.Retryable():
		code = codes.Unavailable
	}
	return status.Errorf(code, "%s: %v", operation, err)
}
