package uninstall

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/mobility/fsm"
)

const DriverName = "csi.shiftpv.io"

type VolumeRepository interface {
	ListVolumes(context.Context) (map[string]volumeapi.State, error)
	ListMoves(context.Context) ([]volumeapi.Move, error)
}

type Checker struct {
	Client           kubernetes.Interface
	Volumes          VolumeRepository
	StorageClassName string
}

type Blocker struct {
	Kind      string
	Namespace string
	Name      string
	Reason    string
}

type Report struct {
	Blockers []Blocker
}

func (r Report) Safe() bool {
	return len(r.Blockers) == 0
}

func (c *Checker) Check(ctx context.Context) (Report, error) {
	if c == nil || c.Client == nil || c.Volumes == nil {
		return Report{}, fmt.Errorf("uninstall checker is not configured")
	}
	if strings.TrimSpace(c.StorageClassName) == "" {
		return Report{}, fmt.Errorf("ShiftPV StorageClass name is required")
	}

	report := Report{}
	persistentVolumes, err := c.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Report{}, fmt.Errorf("list PersistentVolumes: %w", err)
	}
	for _, persistentVolume := range persistentVolumes.Items {
		if persistentVolume.Spec.CSI == nil || persistentVolume.Spec.CSI.Driver != DriverName {
			continue
		}
		reason := fmt.Sprintf("driver=%s volumeHandle=%s", DriverName, persistentVolume.Spec.CSI.VolumeHandle)
		if persistentVolume.Spec.ClaimRef != nil {
			reason += fmt.Sprintf(" claim=%s/%s", persistentVolume.Spec.ClaimRef.Namespace, persistentVolume.Spec.ClaimRef.Name)
		}
		report.Blockers = append(report.Blockers, Blocker{Kind: "PersistentVolume", Name: persistentVolume.Name, Reason: reason})
	}

	claims, err := c.Client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return Report{}, fmt.Errorf("list PersistentVolumeClaims: %w", err)
	}
	for _, claim := range claims.Items {
		if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != c.StorageClassName {
			continue
		}
		reason := "references the ShiftPV StorageClass"
		if claim.Spec.VolumeName != "" {
			reason += " volume=" + claim.Spec.VolumeName
		}
		report.Blockers = append(report.Blockers, Blocker{Kind: "PersistentVolumeClaim", Namespace: claim.Namespace, Name: claim.Name, Reason: reason})
	}

	volumes, err := c.Volumes.ListVolumes(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVVolumes: %w", err)
	}
	for volumeID, state := range volumes {
		reasonParts := []string{"phase=" + state.Phase, "owner=" + state.OwnerNode}
		if state.ActiveMove != "" {
			reasonParts = append(reasonParts, "activeMove="+state.ActiveMove)
		}
		if len(state.PublishedNodes) > 0 {
			reasonParts = append(reasonParts, "publishedNodes="+strings.Join(state.PublishedNodes, ","))
		}
		report.Blockers = append(report.Blockers, Blocker{Kind: "ShiftPVVolume", Name: volumeID, Reason: strings.Join(reasonParts, " ")})
	}

	moves, err := c.Volumes.ListMoves(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list ShiftPVMoves: %w", err)
	}
	for _, move := range moves {
		phase := fsm.Phase(move.Status.Phase)
		if phase == fsm.PhaseSucceeded || phase == fsm.PhaseBlocked {
			continue
		}
		report.Blockers = append(report.Blockers, Blocker{
			Kind:   "ShiftPVMove",
			Name:   move.Name,
			Reason: fmt.Sprintf("phase=%s volume=%s", phase, move.Spec.VolumeID),
		})
	}

	sort.Slice(report.Blockers, func(left, right int) bool {
		leftKey := blockerKey(report.Blockers[left])
		rightKey := blockerKey(report.Blockers[right])
		return leftKey < rightKey
	})
	return report, nil
}

func blockerKey(blocker Blocker) string {
	return blocker.Kind + "\x00" + blocker.Namespace + "\x00" + blocker.Name + "\x00" + blocker.Reason
}
