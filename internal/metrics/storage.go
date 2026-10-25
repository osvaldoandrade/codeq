package metrics

import "github.com/prometheus/client_golang/prometheus"

// Data-volume, admission and engine-health metrics. They describe the local
// node only; every Raft voter exports its own view of its own volume.
var (
	// StorageDataDirTotalBytes is the size of the filesystem holding the data directory.
	StorageDataDirTotalBytes = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "storage_data_dir_total_bytes", Help: "Size of the filesystem holding the CodeQ data directory."})
	// StorageDataDirFreeBytes is the space still available to the (non-root) process.
	StorageDataDirFreeBytes = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "storage_data_dir_free_bytes", Help: "Bytes available to CodeQ on the data directory filesystem."})
	// StorageDataDirUsedRatio is 1 - free/total of the data volume.
	StorageDataDirUsedRatio = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "storage_data_dir_used_ratio", Help: "Used fraction of the data directory filesystem (reserve file included)."})
	// StorageAdmissionThresholdRatio is the configured used fraction that refuses new tasks.
	StorageAdmissionThresholdRatio = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "storage_admission_threshold_ratio", Help: "Configured used fraction at or above which new task creation is refused."})
	// StorageAdmissionEffectiveThresholdRatio is the threshold actually applied:
	// the configured one, lowered to keep room for the next Raft snapshot cycle.
	StorageAdmissionEffectiveThresholdRatio = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "storage_admission_effective_threshold_ratio", Help: "Used fraction at or above which new task creation is refused, after reserving Raft snapshot headroom."})
	// StorageSnapshotEstimateBytes is the expected size of the next Raft snapshot (all shards).
	StorageSnapshotEstimateBytes = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "storage_snapshot_estimate_bytes", Help: "Estimated size of the next Raft snapshot on the data volume, summed over shards."})
	// StorageSnapshotDiskBytes is what Raft snapshots occupy on the data volume now.
	StorageSnapshotDiskBytes = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "storage_snapshot_disk_bytes", Help: "Bytes Raft snapshots (retained and in progress) occupy on the data volume, summed over shards."})
	// RaftSnapshotRetain is the snapshot retention in effect.
	RaftSnapshotRetain = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "raft_snapshot_retain", Help: "Raft snapshots kept on disk after each new snapshot."})
	// StorageAdmissionBlocked is 1 while new task creation is refused.
	StorageAdmissionBlocked = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "storage_admission_blocked", Help: "1 while new task creation is refused by the data volume guard, else 0."})
	// StorageAdmissionRefused counts creates refused with storage_capacity.
	StorageAdmissionRefused = prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "storage_admission_refused_total", Help: "New task creates refused with storage_capacity."})
	// StorageStatErrors counts failed filesystem samples (admission fails open).
	StorageStatErrors = prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "storage_stat_errors_total", Help: "Failed data volume samples; admission fails open while usage is unknown."})
	// StorageReserveBytes is the ballast currently held on the data volume.
	StorageReserveBytes = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "storage_reserve_bytes", Help: "Bytes held by the emergency reserve file on the data volume."})
	// StorageReserveReleased counts emergency releases of the reserve file.
	StorageReserveReleased = prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "storage_reserve_released_total", Help: "Emergency releases of the reserve file on a critically full data volume."})

	// PebbleL0Files is the number of L0 sstables per shard.
	PebbleL0Files = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "pebble_l0_files", Help: "Pebble L0 sstable count per shard."}, []string{shardLabel})
	// PebbleL0Sublevels is the L0 read amplification per shard.
	PebbleL0Sublevels = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "pebble_l0_sublevels", Help: "Pebble L0 sublevel count (read amplification) per shard."}, []string{shardLabel})
	// PebbleCompactionDebtBytes is the estimated compaction debt per shard.
	PebbleCompactionDebtBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "pebble_compaction_debt_bytes", Help: "Pebble estimated bytes to compact for the LSM to reach a stable state, per shard."}, []string{shardLabel})
	// PebbleDiskUsageBytes is the on-disk footprint Pebble reports per shard.
	PebbleDiskUsageBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "pebble_disk_usage_bytes", Help: "Pebble on-disk footprint (sstables, WAL, manifests) per shard."}, []string{shardLabel})
	// PebbleCacheCapacityBytes is the configured block cache per shard.
	PebbleCacheCapacityBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "pebble_cache_capacity_bytes", Help: "Configured Pebble block cache capacity per shard."}, []string{shardLabel})
	// PebbleMemTableBytes is the configured memtable size per shard.
	PebbleMemTableBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: "pebble_memtable_bytes", Help: "Configured Pebble memtable size per shard."}, []string{shardLabel})

	// MemoryContainerLimitBytes is the detected container memory limit (0 = unlimited/unknown).
	MemoryContainerLimitBytes = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "memory_container_limit_bytes", Help: "Detected container memory limit; 0 when unlimited or unknown."})
	// MemoryPebbleSizingFloored is 1 when the container limit is too small for
	// the shard count and a per-shard Pebble size was raised to its floor.
	MemoryPebbleSizingFloored = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "memory_pebble_sizing_floored", Help: "1 when derived Pebble cache/memtable sizes hit their per-shard floors because the memory limit is too small for the shard count, else 0."})
	// MemoryPebbleMinSafeLimitBytes is the smallest limit that avoids the floors.
	MemoryPebbleMinSafeLimitBytes = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "memory_pebble_min_safe_limit_bytes", Help: "Smallest container memory limit at which derived Pebble sizes stay above their per-shard floors for the configured shard count."})
	// MemoryGoLimitBytes is the Go runtime soft memory limit in effect.
	MemoryGoLimitBytes = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "memory_go_limit_bytes", Help: "Go runtime soft memory limit (GOMEMLIMIT) in effect."})
)

func init() {
	prometheus.MustRegister(
		StorageDataDirTotalBytes, StorageDataDirFreeBytes, StorageDataDirUsedRatio,
		StorageAdmissionThresholdRatio, StorageAdmissionEffectiveThresholdRatio,
		StorageAdmissionBlocked, StorageAdmissionRefused,
		StorageSnapshotEstimateBytes, StorageSnapshotDiskBytes, RaftSnapshotRetain,
		StorageStatErrors, StorageReserveBytes, StorageReserveReleased,
		PebbleL0Files, PebbleL0Sublevels, PebbleCompactionDebtBytes, PebbleDiskUsageBytes,
		PebbleCacheCapacityBytes, PebbleMemTableBytes,
		MemoryContainerLimitBytes, MemoryGoLimitBytes,
		MemoryPebbleSizingFloored, MemoryPebbleMinSafeLimitBytes,
	)
}
