package capacity

import (
	"fmt"
	"math"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

const ReservationSelector = "app.kubernetes.io/name=shiftpv,app.kubernetes.io/component=volume-reservation"

// ReservedBytes counts owner reservations and approved incoming moves once.
func ReservedBytes(reservations []corev1.ConfigMap, volumes map[string]volumeapi.State, moves []volumeapi.Move, nodeName string) (int64, error) {
	seen := make(map[string]struct{}, len(reservations))
	var total int64
	for index := range reservations {
		reservation := &reservations[index]
		volumeID := reservation.Data["volumeID"]
		if volumeID == "" || volumeID != reservation.Name {
			return 0, fmt.Errorf("reservation %q has invalid volume identity", reservation.Name)
		}
		seen[volumeID] = struct{}{}
		ownerNode := reservation.Data["nodeName"]
		if state, exists := volumes[volumeID]; exists {
			if state.UID == "" || reservation.Data["volumeUID"] != state.UID {
				return 0, fmt.Errorf("reservation %q does not match the current volume incarnation", reservation.Name)
			}
			ownerNode = state.OwnerNode
		}
		if ownerNode == "" {
			return 0, fmt.Errorf("reservation %q has no current owner", reservation.Name)
		}
		if ownerNode != nodeName {
			continue
		}
		capacityBytes, parseErr := strconv.ParseInt(reservation.Data["capacity"], 10, 64)
		if parseErr != nil || capacityBytes <= 0 {
			return 0, fmt.Errorf("reservation %q has invalid capacity", reservation.Name)
		}
		if total > math.MaxInt64-capacityBytes {
			return 0, fmt.Errorf("Pool reservation total overflows int64")
		}
		total += capacityBytes
	}
	for volumeID, state := range volumes {
		if state.OwnerNode != nodeName {
			continue
		}
		if _, exists := seen[volumeID]; !exists {
			return 0, fmt.Errorf("volume %q has no capacity reservation", volumeID)
		}
	}
	for _, move := range moves {
		if !move.Status.CapacityApproved || move.Status.DestinationNode != nodeName {
			continue
		}
		reservation, reservationExists := reservationByID(reservations, move.Spec.VolumeID)
		state, exists := volumes[move.Spec.VolumeID]
		if !exists {
			// A completed DeleteVolume removes both the volume state and its
			// reservation, while the terminal Move remains as an audit record.
			// Such a record owns no capacity and must not block unrelated PVCs.
			if !reservationExists {
				continue
			}
			return 0, fmt.Errorf("move %q has no volume state", move.Name)
		}
		if !volumeapi.MoveReservesDestination(move, state, nodeName) {
			continue
		}
		if !reservationExists {
			return 0, fmt.Errorf("move %q has no capacity reservation", move.Name)
		}
		capacityBytes, parseErr := strconv.ParseInt(reservation.Data["capacity"], 10, 64)
		if parseErr != nil || capacityBytes <= 0 || total > math.MaxInt64-capacityBytes {
			return 0, fmt.Errorf("move %q has invalid capacity reservation", move.Name)
		}
		total += capacityBytes
	}
	return total, nil
}

func reservationByID(items []corev1.ConfigMap, volumeID string) (*corev1.ConfigMap, bool) {
	for index := range items {
		if items[index].Name == volumeID {
			return &items[index], true
		}
	}
	return nil, false
}
