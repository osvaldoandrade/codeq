package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Storage and memory safety defaults applied by the config loaders. A
// Config built in code without the loader keeps the guard disabled (zero
// AdmissionMaxUsedFraction) so embedders and tests are unaffected.
const (
	DefaultStorageAdmissionMaxUsedFraction = 0.80
	DefaultStorageReserveBytes             = int64(64 << 20)
	DefaultStorageCheckIntervalSeconds     = 5
	DefaultGoMemoryLimitFraction           = 0.70
	DefaultPebbleL0StopWritesThreshold     = 64
	// DefaultRaftSnapshotRetain is 0 = auto: the server keeps one snapshot
	// on data volumes of at most 1 GiB and two on larger ones.
	DefaultRaftSnapshotRetain = 0
	DefaultRaftTrailingLogs   = 2048

	maxPebbleMemTableBytes = int64(4 << 30)
)

// StorageConfig protects the local data volume (see internal/storage/diskguard).
type StorageConfig struct {
	// AdmissionMaxUsedFraction refuses NEW task creation with HTTP 429
	// storage_capacity while the data volume is at or above this used
	// fraction, in (0, 1]. Claims, completions, lease renewals, deletions,
	// cleanup and Raft replication are never refused. Zero disables the
	// guard (loader default 0.80). Env STORAGE_ADMISSION_MAX_USED_FRACTION.
	AdmissionMaxUsedFraction float64 `yaml:"admissionMaxUsedFraction"`
	// ReserveBytes is the emergency ballast kept in the data directory,
	// capped to 5% of the volume in whole MiB, released when the volume is
	// critically full. Unset uses 64 MiB; 0 disables and removes it.
	// Env STORAGE_RESERVE_BYTES.
	ReserveBytes *int64 `yaml:"reserveBytes"`
	// CheckIntervalSeconds is the sampling period and the maximum age of
	// an admission decision (default 5). Env STORAGE_CHECK_INTERVAL_SECONDS.
	CheckIntervalSeconds int `yaml:"checkIntervalSeconds"`
}

// MemoryConfig sizes memory consumers from the container limit (see internal/memlimit).
type MemoryConfig struct {
	// LimitBytes overrides the detected cgroup memory limit; 0 detects it
	// (cgroup v2 memory.max, then v1). Env MEMORY_LIMIT_BYTES.
	LimitBytes int64 `yaml:"limitBytes"`
	// GoMemoryLimitFraction sets the Go soft memory limit to this fraction
	// of the container limit, in (0, 1] (default 0.70). An explicit
	// GOMEMLIMIT always wins. Env GO_MEMORY_LIMIT_FRACTION.
	GoMemoryLimitFraction float64 `yaml:"goMemoryLimitFraction"`
	// PebbleCacheBytes is the block cache per Pebble shard; 0 derives 15%
	// of the limit within [8 MiB, 256 MiB]. Env PEBBLE_CACHE_BYTES.
	PebbleCacheBytes int64 `yaml:"pebbleCacheBytes"`
	// PebbleMemTableBytes is the memtable size per Pebble shard; 0 derives
	// 1/32 of the limit within [4 MiB, 64 MiB]. Env PEBBLE_MEMTABLE_BYTES.
	PebbleMemTableBytes int64 `yaml:"pebbleMemTableBytes"`
	// PebbleL0StopWritesThreshold stops writes at this many L0 files
	// (default 64, unchanged). Env PEBBLE_L0_STOP_WRITES_THRESHOLD.
	PebbleL0StopWritesThreshold int `yaml:"pebbleL0StopWritesThreshold"`
}

// applyStorageEnvAndDefaults is called by applyEnvAndDefaults. A malformed
// value becomes -1 so Validate rejects it instead of silently defaulting.
func applyStorageEnvAndDefaults(c *Config) {
	envFloat("STORAGE_ADMISSION_MAX_USED_FRACTION", &c.Storage.AdmissionMaxUsedFraction)
	if v, ok := lookupNonEmpty("STORAGE_RESERVE_BYTES"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			n = -1
		}
		c.Storage.ReserveBytes = &n
	}
	envInt("STORAGE_CHECK_INTERVAL_SECONDS", &c.Storage.CheckIntervalSeconds)
	envInt64("MEMORY_LIMIT_BYTES", &c.Memory.LimitBytes)
	envFloat("GO_MEMORY_LIMIT_FRACTION", &c.Memory.GoMemoryLimitFraction)
	envInt64("PEBBLE_CACHE_BYTES", &c.Memory.PebbleCacheBytes)
	envInt64("PEBBLE_MEMTABLE_BYTES", &c.Memory.PebbleMemTableBytes)
	envInt("PEBBLE_L0_STOP_WRITES_THRESHOLD", &c.Memory.PebbleL0StopWritesThreshold)
	envInt("RAFT_SNAPSHOT_RETAIN", &c.Raft.SnapshotRetain)
	envInt("RAFT_TRAILING_LOGS", &c.Raft.TrailingLogs)

	if c.Storage.AdmissionMaxUsedFraction == 0 {
		c.Storage.AdmissionMaxUsedFraction = DefaultStorageAdmissionMaxUsedFraction
	}
	if c.Storage.ReserveBytes == nil {
		n := DefaultStorageReserveBytes
		c.Storage.ReserveBytes = &n
	}
	setDefaultInt(&c.Storage.CheckIntervalSeconds, DefaultStorageCheckIntervalSeconds)
	if c.Memory.GoMemoryLimitFraction == 0 {
		c.Memory.GoMemoryLimitFraction = DefaultGoMemoryLimitFraction
	}
	setDefaultInt(&c.Memory.PebbleL0StopWritesThreshold, DefaultPebbleL0StopWritesThreshold)
	// raft.snapshotRetain stays 0 (auto) unless set; the server resolves it
	// from the data volume size (see internal/raft.AutoSnapshotRetain).
	setDefaultInt(&c.Raft.TrailingLogs, DefaultRaftTrailingLogs)
}

// validateStorage reports out-of-range storage, memory and Raft retention
// settings. Zero always means "default/derived" and is valid.
func validateStorage(c *Config) []string {
	var errs []string
	add := func(bad bool, msg string) {
		if bad {
			errs = append(errs, msg)
		}
	}
	add(!fractionOK(c.Storage.AdmissionMaxUsedFraction), "storage.admissionMaxUsedFraction must be within (0, 1]")
	add(c.Storage.ReserveBytes != nil && *c.Storage.ReserveBytes < 0, "storage.reserveBytes must be >= 0")
	add(c.Storage.CheckIntervalSeconds < 0, "storage.checkIntervalSeconds must be >= 1")
	add(c.Memory.LimitBytes < 0, "memory.limitBytes must be >= 0")
	add(!fractionOK(c.Memory.GoMemoryLimitFraction), "memory.goMemoryLimitFraction must be within (0, 1]")
	add(c.Memory.PebbleCacheBytes < 0, "memory.pebbleCacheBytes must be >= 0")
	add(c.Memory.PebbleMemTableBytes < 0 || c.Memory.PebbleMemTableBytes >= maxPebbleMemTableBytes,
		fmt.Sprintf("memory.pebbleMemTableBytes must be within [0, %d)", maxPebbleMemTableBytes))
	add(c.Memory.PebbleL0StopWritesThreshold < 0, "memory.pebbleL0StopWritesThreshold must be >= 1")
	add(c.Raft.SnapshotRetain < 0, "raft.snapshotRetain must be >= 1 (or 0 for auto)")
	add(c.Raft.TrailingLogs < 0, "raft.trailingLogs must be >= 1")
	return errs
}

func fractionOK(f float64) bool { return f >= 0 && f <= 1 }

func lookupNonEmpty(key string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	return v, v != ""
}

func envInt(key string, dst *int) {
	if v, ok := lookupNonEmpty(key); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			n = -1
		}
		*dst = n
	}
}

func envInt64(key string, dst *int64) {
	if v, ok := lookupNonEmpty(key); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			n = -1
		}
		*dst = n
	}
}

func envFloat(key string, dst *float64) {
	if v, ok := lookupNonEmpty(key); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			f = -1
		}
		*dst = f
	}
}

func setDefaultInt(dst *int, def int) {
	if *dst == 0 {
		*dst = def
	}
}
