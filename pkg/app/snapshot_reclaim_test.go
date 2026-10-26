package app

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	hraft "github.com/hashicorp/raft"

	"github.com/osvaldoandrade/codeq/internal/storage/diskguard"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// seedOldRetention leaves what a release with retain=3 left on a voter that
// died mid-snapshot: three complete snapshots and one abandoned .tmp.
func seedOldRetention(t *testing.T, shardPath string) *hraft.FileSnapshotStore {
	t.Helper()
	store, err := hraft.NewFileSnapshotStore(filepath.Join(shardPath, "snapshots"), 3, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []uint64{10, 20, 30} {
		sink, err := store.Create(hraft.SnapshotVersionMax, index, 1, hraft.Configuration{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = sink.Write(bytes.Repeat([]byte{7}, 32<<10)); err != nil {
			t.Fatal(err)
		}
		if err = sink.Close(); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(shardPath, "snapshots", "snapshots", "1-40-1700000000000.tmp")
	if err = os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(tmp, "state.bin"), make([]byte, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	return store
}

func reclaimRuntime(path string, shards int, raftEnabled bool) *pebbleRuntime {
	cfg := &config.Config{}
	cfg.Raft.Enabled = raftEnabled
	return &pebbleRuntime{cfg: cfg, pc: pebbleConfig{Path: path, NumShards: shards}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func snapshotCount(t *testing.T, store *hraft.FileSnapshotStore) int {
	t.Helper()
	list, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func TestStartupReclaimsSnapshotsOfOlderRetentionBeforePebbleOpens(t *testing.T) {
	// A 1 GiB volume selects the small-volume auto retention (1).
	useStorageStat(t, func(string) (diskguard.Usage, error) {
		return diskguard.Usage{TotalBytes: 1 << 30, FreeBytes: 0}, nil
	})
	path := t.TempDir()
	shard1 := filepath.Join(path, "shard1")
	stores := []*hraft.FileSnapshotStore{seedOldRetention(t, filepath.Join(path, "shard0")), seedOldRetention(t, shard1)}

	reclaimRuntime(path, 2, true).reclaimSnapshotSpace()

	for i, store := range stores {
		list, err := store.List()
		if err != nil || len(list) != 1 || list[0].Index != 30 {
			t.Fatalf("shard %d snapshots = %+v, %v; want only index 30", i, list, err)
		}
	}
	if _, err := os.Stat(filepath.Join(shard1, "snapshots", "snapshots", "1-40-1700000000000.tmp")); !os.IsNotExist(err) {
		t.Fatalf("abandoned snapshot survived startup: %v", err)
	}
}

func TestStartupSnapshotReclaimSkippedWithoutRaft(t *testing.T) {
	useStorageStat(t, func(string) (diskguard.Usage, error) {
		return diskguard.Usage{TotalBytes: 1 << 30, FreeBytes: 0}, nil
	})
	path := t.TempDir()
	store := seedOldRetention(t, path)

	reclaimRuntime(path, 1, false).reclaimSnapshotSpace()

	if n := snapshotCount(t, store); n != 3 {
		t.Fatalf("snapshots without raft = %d, want 3 untouched", n)
	}
}
