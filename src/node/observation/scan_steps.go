//go:build linux || darwin

package observation

import (
	"context"
	"path/filepath"

	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
	"github.com/project-jelly/ShiftPV/src/node/ownership"
)

// This file holds the per-step halves of Scan. Scan itself keeps the ownership
// store and the inventory handle so their lifetimes are unchanged, and calls
// these helpers in the order the original body ran them; every guard, early
// return and message string is kept exactly as it was.

// configurationInvalid repeats the fail-closed precondition of an inventory
// scan: a scanner that cannot name an installation, a publication, an absolute
// host or target root, a pool, or a page budget within 1..256 observes nothing.
func (s *Scanner) configurationInvalid(pool volumeapi.Pool) bool {
	return s == nil || s.Installation == nil || s.Publications == nil || !filepath.IsAbs(s.HostRoot) ||
		!filepath.IsAbs(s.TargetRoot) || pool.UID == "" || s.Limit < 1 || s.Limit > 256
}

// observeRecorded pages the recorded placements into result until the limit is
// reached, then probes one more page to learn whether anything was left behind.
// It reports false when a read failed and result already carries the message.
func (s *Scanner) observeRecorded(ctx context.Context, inventory *ownership.Inventory, pool volumeapi.Pool,
	root, installationID string, known map[string]struct{}, result *volumeapi.PoolInventory) bool {
	done := false
	for !done && len(result.Copies) < s.Limit {
		page, pageDone, pageErr := inventory.Page(ctx, min(64, s.Limit-len(result.Copies)))
		if pageErr != nil {
			result.Message = "InventoryReadFailed: " + pageErr.Error()
			return false
		}
		for _, item := range page {
			result.Copies = append(result.Copies, s.observeItem(item, pool, root, installationID))
			if item.Identity != nil && item.Present {
				known[physicalKey(*item.Identity)] = struct{}{}
			}
		}
		done = pageDone
	}
	for !done && len(result.Copies) == s.Limit {
		page, pageDone, pageErr := inventory.Page(ctx, 1)
		if pageErr != nil {
			result.Message = "InventoryReadFailed: " + pageErr.Error()
			return false
		}
		if len(page) > 0 {
			break
		}
		done = pageDone
	}
	result.Truncated = !done
	return true
}

// observeItem turns one recorded placement into a copy observation: a placement
// registered to another installation, pool or node loses its identity, and only
// an intact present copy is asked whether it is still published.
func (s *Scanner) observeItem(item ownership.Observation, pool volumeapi.Pool, root, installationID string) volumeapi.CopyObservation {
	observation := volumeapi.CopyObservation{Marker: item.Marker, Identity: item.Identity, Present: item.Present, Problem: item.Problem}
	if item.Identity != nil && (item.Identity.InstallationID != installationID || item.Identity.PoolName != pool.Name ||
		item.Identity.PoolUID != pool.UID || item.Identity.NodeName != pool.NodeName) {
		observation.Identity = nil
		observation.Problem = "PoolIdentityMismatch"
	}
	if observation.Identity != nil && observation.Present && observation.Problem == "" {
		source := filepath.Join(root, filepath.FromSlash(physicalKey(*observation.Identity)))
		published, err := s.Publications.HasPublishedTarget(source, s.TargetRoot)
		observation.Published = published
		if err != nil {
			observation.Problem = "PublicationObservationFailed: " + err.Error()
		}
	}
	return observation
}

// copyProblemMessage summarizes an otherwise clean inventory: a single troubled
// copy is enough to withhold validity from the whole pool observation.
func copyProblemMessage(copies []volumeapi.CopyObservation) string {
	for _, observed := range copies {
		if observed.Problem != "" {
			return "CopyObservationProblem"
		}
	}
	return ""
}
