package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	raftpkg "github.com/osvaldoandrade/codeq/internal/raft"
	"github.com/osvaldoandrade/codeq/internal/storage/diskguard"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// gibVolume reports a volume of a fixed size whose used share the test sets.
type gibVolume struct {
	total   uint64
	usedMiB atomic.Int64
}

func (v *gibVolume) stat(string) (diskguard.Usage, error) {
	used := uint64(v.usedMiB.Load()) << 20 // #nosec G115 -- small test value
	return diskguard.Usage{TotalBytes: v.total, FreeBytes: v.total - used}, nil
}

func TestPebbleSizingWarnsWhenLimitIsBelowShardFloors(t *testing.T) {
	var logs bytes.Buffer
	rt := &pebbleRuntime{
		cfg:    &config.Config{Memory: config.MemoryConfig{LimitBytes: 256 << 20}},
		pc:     pebbleConfig{NumShards: 8},
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	sizing := rt.pebbleSizing() // must not fail or panic
	if sizing.CacheBytes != 8<<20 || sizing.MemTableBytes != 4<<20 {
		t.Fatalf("floored sizing = %+v", sizing)
	}
	if v, ok := gaugeValue(t, "codeq_memory_pebble_sizing_floored", ""); !ok || v != 1 {
		t.Fatalf("floored gauge = %v (%v)", v, ok)
	}
	if v, _ := gaugeValue(t, "codeq_memory_pebble_min_safe_limit_bytes", ""); v != 1<<30 {
		t.Fatalf("min safe limit = %v, want 1 GiB", v)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "below the safe floor") ||
		!strings.Contains(out, "minSafeLimitBytes=1073741824") {
		t.Fatalf("missing floor warning:\n%s", out)
	}

	logs.Reset()
	rt.cfg.Memory.LimitBytes = 512 << 20
	rt.pc.NumShards = 1
	_ = rt.pebbleSizing()
	if v, _ := gaugeValue(t, "codeq_memory_pebble_sizing_floored", ""); v != 0 {
		t.Fatalf("floored gauge not cleared: %v", v)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("unexpected warning at a safe limit:\n%s", logs.String())
	}
}

func TestSnapshotRetainResolvesFromVolumeSize(t *testing.T) {
	rt := &pebbleRuntime{cfg: &config.Config{}, pc: pebbleConfig{Path: t.TempDir()}, logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	vol := &gibVolume{total: 1 << 30}
	useStorageStat(t, vol.stat)
	if got := rt.snapshotRetain(); got != 1 {
		t.Fatalf("1 GiB volume retain = %d, want 1", got)
	}
	vol.total = 10 << 30
	if got := rt.snapshotRetain(); got != raftpkg.DefaultSnapshotRetain {
		t.Fatalf("10 GiB volume retain = %d", got)
	}
	rt.cfg.Raft.SnapshotRetain = 3
	if got := rt.snapshotRetain(); got != 3 {
		t.Fatalf("explicit retain ignored: %d", got)
	}
	rt.cfg.Raft.SnapshotRetain = 0
	useStorageStat(t, func(string) (diskguard.Usage, error) { return diskguard.Usage{}, errors.New("statfs failed") })
	if got := rt.snapshotRetain(); got != raftpkg.DefaultSnapshotRetain {
		t.Fatalf("unknown volume retain = %d", got)
	}
}

// TestRaftSnapshotHeadroomRefusesCreates boots a single-node Raft voter on
// a simulated 1 GiB volume: retention resolves to 1, and once a 300 MiB
// snapshot exists the admission threshold drops from 80% to
// 1 - 300 MiB / 1 GiB, refusing new tasks at 75% used.
func TestRaftSnapshotHeadroomRefusesCreates(t *testing.T) {
	vol := &gibVolume{total: 1 << 30}
	vol.usedMiB.Store(768) // 75%: below the configured 80%
	useStorageStat(t, vol.stat)
	path := t.TempDir() + "/pebble"
	noReserve := int64(0)
	srv := newStorageGuardServer(t, path, func(c *config.Config) {
		c.Storage.ReserveBytes = &noReserve
		c.Raft = config.RaftConfig{
			Enabled: true, SelfID: topicNodeOne, BindAddr: net.JoinHostPort("127.0.0.1", "0"), Bootstrap: true,
			HeartbeatMS: 50, ElectionMS: 50, LeaderLeaseMS: 50, CommitMS: 10, ApplyTimeoutSeconds: 2,
		}
	})
	if v, _ := gaugeValue(t, "codeq_raft_snapshot_retain", ""); v != 1 {
		t.Fatalf("raft snapshot retain gauge = %v, want 1", v)
	}
	ctx := context.Background()
	create := func(n int) (int, string) {
		body := map[string]any{keyCommand: storageGuardCommand, keyPayload: map[string]int{"n": n}}
		return doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, nil)
	}
	waitFor(t, "leader accepts creates", func() bool { status, _ := create(1); return status == http.StatusAccepted })

	// A 300 MiB snapshot appears (sparse: no real disk is consumed).
	state := filepath.Join(path, "snapshots", "snapshots", "9-999999-1", "state.bin")
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(state) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(300 << 20); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	want := 1 - float64(300<<20)/float64(1<<30)
	waitFor(t, "effective threshold lowered", func() bool {
		v, _ := gaugeValue(t, "codeq_storage_admission_effective_threshold_ratio", "")
		return v > want-1e-9 && v < want+1e-9
	})
	if v, _ := gaugeValue(t, "codeq_storage_snapshot_estimate_bytes", ""); v < 300<<20 {
		t.Fatalf("snapshot estimate = %v", v)
	}
	time.Sleep(1100 * time.Millisecond) // let the 1s admission sample expire
	if status, raw := create(2); status != http.StatusTooManyRequests || !strings.Contains(raw, domain.StorageCapacityCode) {
		t.Fatalf("create inside snapshot headroom: %d %s", status, raw)
	}

	// Removing the snapshot restores the configured threshold.
	if err := os.RemoveAll(filepath.Dir(state)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "admission reopened", func() bool { status, _ := create(3); return status == http.StatusAccepted })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
