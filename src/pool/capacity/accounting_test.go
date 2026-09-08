package capacity

import (
	"math"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

func TestReservedBytes(t *testing.T) {
	cm := func(id, node string, n int64) corev1.ConfigMap {
		return corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: id}, Data: map[string]string{"volumeID": id, "nodeName": node, "capacity": strconv.FormatInt(n, 10)}}
	}
	move := volumeapi.Move{Name: "m", Spec: volumeapi.MoveSpec{VolumeID: "v"}, Status: volumeapi.MoveStatus{DestinationNode: "b", CapacityApproved: true}}
	for _, tc := range []struct {
		name         string
		reservations []corev1.ConfigMap
		volumes      map[string]volumeapi.State
		moves        []volumeapi.Move
		node         string
		want         int64
		invalid      bool
	}{
		{name: "empty", node: "a"},
		{name: "owner", reservations: []corev1.ConfigMap{cm("v", "a", 64)}, node: "a", want: 64},
		{name: "other pool", reservations: []corev1.ConfigMap{cm("v", "a", 64)}, node: "b"},
		{name: "owner committed", reservations: []corev1.ConfigMap{cm("v", "a", 64)}, volumes: map[string]volumeapi.State{"v": {OwnerNode: "b", ActiveMove: "m"}}, moves: []volumeapi.Move{move}, node: "b", want: 64},
		{name: "incoming", reservations: []corev1.ConfigMap{cm("v", "a", 64)}, volumes: map[string]volumeapi.State{"v": {OwnerNode: "a", ActiveMove: "m"}}, moves: []volumeapi.Move{move}, node: "b", want: 64},
		{name: "inactive move", reservations: []corev1.ConfigMap{cm("v", "a", 64)}, volumes: map[string]volumeapi.State{"v": {OwnerNode: "a"}}, moves: []volumeapi.Move{move}, node: "b"},
		{name: "deleted history", moves: []volumeapi.Move{move}, node: "b"},
		{name: "missing state", reservations: []corev1.ConfigMap{cm("v", "a", 64)}, moves: []volumeapi.Move{move}, node: "b", invalid: true},
		{name: "missing reservation", volumes: map[string]volumeapi.State{"v": {OwnerNode: "a"}}, node: "a", invalid: true},
		{name: "incoming missing reservation", volumes: map[string]volumeapi.State{"v": {OwnerNode: "a", ActiveMove: "m"}}, moves: []volumeapi.Move{move}, node: "b", invalid: true},
		{name: "missing identity", reservations: []corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Name: "v"}}}, node: "a", invalid: true},
		{name: "missing owner", reservations: []corev1.ConfigMap{cm("v", "", 64)}, node: "a", invalid: true},
		{name: "zero capacity", reservations: []corev1.ConfigMap{cm("v", "a", 0)}, node: "a", invalid: true},
		{name: "invalid incoming", reservations: []corev1.ConfigMap{cm("v", "a", 0)}, volumes: map[string]volumeapi.State{"v": {OwnerNode: "a", ActiveMove: "m"}}, moves: []volumeapi.Move{move}, node: "b", invalid: true},
		{name: "overflow", reservations: []corev1.ConfigMap{cm("v", "a", math.MaxInt64), cm("w", "a", 1)}, node: "a", invalid: true},
		{name: "incoming overflow", reservations: []corev1.ConfigMap{cm("v", "a", 1), cm("w", "b", math.MaxInt64)}, volumes: map[string]volumeapi.State{"v": {OwnerNode: "a", ActiveMove: "m"}}, moves: []volumeapi.Move{move}, node: "b", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReservedBytes(tc.reservations, tc.volumes, tc.moves, tc.node)
			if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
				t.Fatalf("got %d/%v want %d invalid=%v", got, err, tc.want, tc.invalid)
			}
		})
	}
}
