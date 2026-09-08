package metrics

import (
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	"github.com/cagojeiger/ShiftPV/src/pool/readiness"
)

// ObservePool reuses the existing probe's statfs result, including on a full or read-only filesystem.
func (e *Exporter) ObservePool(pool volumeapi.Pool, result readiness.Result, err error) {
	if err != nil {
		e.Cache.update("filesystem", nil, false)
		return
	}
	if pool.Name == "" {
		e.Cache.clear("filesystem")
		return
	}
	if !result.CapacityReadable.OK {
		e.Cache.update("filesystem", nil, false)
		return
	}
	labels := []string{pool.Name, pool.NodeName}
	e.Cache.update("filesystem", []sample{
		{"pool_filesystem_size_bytes", float64(result.Filesystem.TotalBytes), labels},
		{"pool_filesystem_available_bytes", float64(result.Filesystem.AvailableBytes), labels},
		{"pool_filesystem_available_inodes", float64(result.Filesystem.AvailableInodes), labels},
	}, true)
}
