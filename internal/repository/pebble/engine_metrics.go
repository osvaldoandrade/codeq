package pebble

import "github.com/osvaldoandrade/codeq/internal/metrics"

// EngineStats is the bounded subset of Pebble health the server exports.
type EngineStats struct {
	L0Files             int64
	L0Sublevels         int32
	CompactionDebtBytes uint64
	DiskUsageBytes      uint64
}

// EngineStats samples Pebble's metrics. It takes Pebble's internal mutex
// briefly, so callers sample on a timer, never per request.
func (d *DB) EngineStats() EngineStats {
	if d == nil || d.db == nil {
		return EngineStats{}
	}
	m := d.db.Metrics()
	return EngineStats{
		L0Files:             m.Levels[0].NumFiles,
		L0Sublevels:         m.Levels[0].Sublevels,
		CompactionDebtBytes: m.Compact.EstimatedDebt,
		DiskUsageBytes:      m.DiskSpaceUsage(),
	}
}

// ObserveEngineMetrics publishes EngineStats under the shard label.
func (d *DB) ObserveEngineMetrics(shard string) {
	s := d.EngineStats()
	metrics.PebbleL0Files.WithLabelValues(shard).Set(float64(s.L0Files))
	metrics.PebbleL0Sublevels.WithLabelValues(shard).Set(float64(s.L0Sublevels))
	metrics.PebbleCompactionDebtBytes.WithLabelValues(shard).Set(float64(s.CompactionDebtBytes))
	metrics.PebbleDiskUsageBytes.WithLabelValues(shard).Set(float64(s.DiskUsageBytes))
}
