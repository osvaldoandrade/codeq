package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageDefaultsFromLoader(t *testing.T) {
	cfg, err := LoadConfigOptional("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.AdmissionMaxUsedFraction != 0.80 {
		t.Errorf("admission fraction = %v", cfg.Storage.AdmissionMaxUsedFraction)
	}
	if cfg.Storage.ReserveBytes == nil || *cfg.Storage.ReserveBytes != 64<<20 {
		t.Errorf("reserve = %v", cfg.Storage.ReserveBytes)
	}
	if cfg.Storage.CheckIntervalSeconds != 5 {
		t.Errorf("interval = %d", cfg.Storage.CheckIntervalSeconds)
	}
	if cfg.Memory.GoMemoryLimitFraction != 0.70 || cfg.Memory.LimitBytes != 0 ||
		cfg.Memory.PebbleCacheBytes != 0 || cfg.Memory.PebbleMemTableBytes != 0 || cfg.Memory.PebbleL0StopWritesThreshold != 64 {
		t.Errorf("memory defaults = %+v", cfg.Memory)
	}
	if cfg.Raft.SnapshotRetain != 2 || cfg.Raft.TrailingLogs != 2048 {
		t.Errorf("raft retention = %d/%d", cfg.Raft.SnapshotRetain, cfg.Raft.TrailingLogs)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestStorageEnvOverrides(t *testing.T) {
	t.Setenv("STORAGE_ADMISSION_MAX_USED_FRACTION", "0.7")
	t.Setenv("STORAGE_RESERVE_BYTES", "0")
	t.Setenv("STORAGE_CHECK_INTERVAL_SECONDS", "2")
	t.Setenv("MEMORY_LIMIT_BYTES", "536870912")
	t.Setenv("GO_MEMORY_LIMIT_FRACTION", "0.6")
	t.Setenv("PEBBLE_CACHE_BYTES", "33554432")
	t.Setenv("PEBBLE_MEMTABLE_BYTES", "8388608")
	t.Setenv("PEBBLE_L0_STOP_WRITES_THRESHOLD", "24")
	t.Setenv("RAFT_SNAPSHOT_RETAIN", "1")
	t.Setenv("RAFT_TRAILING_LOGS", "1024")
	cfg, err := LoadConfigOptional("")
	if err != nil {
		t.Fatal(err)
	}
	s, m := cfg.Storage, cfg.Memory
	if s.AdmissionMaxUsedFraction != 0.7 || s.ReserveBytes == nil || *s.ReserveBytes != 0 || s.CheckIntervalSeconds != 2 {
		t.Errorf("storage = %+v reserve=%v", s, s.ReserveBytes)
	}
	if m.LimitBytes != 512<<20 || m.GoMemoryLimitFraction != 0.6 || m.PebbleCacheBytes != 32<<20 || m.PebbleMemTableBytes != 8<<20 || m.PebbleL0StopWritesThreshold != 24 {
		t.Errorf("memory = %+v", m)
	}
	if cfg.Raft.SnapshotRetain != 1 || cfg.Raft.TrailingLogs != 1024 {
		t.Errorf("raft = %d/%d", cfg.Raft.SnapshotRetain, cfg.Raft.TrailingLogs)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid overrides rejected: %v", err)
	}
}

func TestStorageYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codeq.yaml")
	yaml := `storage:
  admissionMaxUsedFraction: 0.9
  reserveBytes: 0
  checkIntervalSeconds: 10
memory:
  goMemoryLimitFraction: 0.5
  pebbleCacheBytes: 16777216
raft:
  snapshotRetain: 3
  trailingLogs: 4096
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigOptional(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.AdmissionMaxUsedFraction != 0.9 || *cfg.Storage.ReserveBytes != 0 || cfg.Storage.CheckIntervalSeconds != 10 {
		t.Errorf("storage = %+v", cfg.Storage)
	}
	if cfg.Memory.GoMemoryLimitFraction != 0.5 || cfg.Memory.PebbleCacheBytes != 16<<20 {
		t.Errorf("memory = %+v", cfg.Memory)
	}
	if cfg.Raft.SnapshotRetain != 3 || cfg.Raft.TrailingLogs != 4096 {
		t.Errorf("raft = %+v", cfg.Raft)
	}
}

func TestStorageInvalidEnvFailsValidation(t *testing.T) {
	for key, value := range map[string]string{
		"STORAGE_ADMISSION_MAX_USED_FRACTION": "1.5",
		"STORAGE_RESERVE_BYTES":               "lots",
		"STORAGE_CHECK_INTERVAL_SECONDS":      "soon",
		"MEMORY_LIMIT_BYTES":                  "-5",
		"GO_MEMORY_LIMIT_FRACTION":            "NaN",
		"PEBBLE_CACHE_BYTES":                  "big",
		"PEBBLE_MEMTABLE_BYTES":               "4294967296",
		"PEBBLE_L0_STOP_WRITES_THRESHOLD":     "x",
		"RAFT_SNAPSHOT_RETAIN":                "-1",
		"RAFT_TRAILING_LOGS":                  "many",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			cfg, err := LoadConfigOptional("")
			if err != nil {
				t.Fatal(err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatalf("%s=%q accepted", key, value)
			}
			if !strings.Contains(err.Error(), "must be") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestStorageZeroValueConfigIsValid(t *testing.T) {
	// A Config built in code (embedders, tests) keeps the guard disabled
	// and must not be rejected for omitting the new blocks.
	var cfg Config
	if errs := validateStorage(&cfg); len(errs) != 0 {
		t.Fatalf("zero config rejected: %v", errs)
	}
}
