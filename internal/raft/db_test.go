package raft

import (
	"context"
	"errors"
	"testing"
	"time"

	pebbledb "github.com/cockroachdb/pebble"
)

// openSingleNodeDB brings up a 1-node raft cluster on an ephemeral
// port. The node bootstraps itself as the sole voter and elects within
// ~50ms. Useful smoke for the wrapper integration without needing the
// full 3-node setup from T9.
func openSingleNodeDB(t *testing.T) *DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	d, err := Open(ctx, Config{
		Path:          t.TempDir(),
		SelfID:        "node-1",
		BindAddr:      "127.0.0.1:0",
		Bootstrap:     true,
		HeartbeatMS:   50,
		ElectionMS:    50,
		LeaderLeaseMS: 50,
		CommitMS:      10,
		ApplyTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := d.WaitLeader(ctx); err != nil {
		t.Fatalf("WaitLeader: %v", err)
	}
	return d
}

func TestDB_SingleNodeBootstrapElects(t *testing.T) {
	d := openSingleNodeDB(t)
	if !d.IsLeader() {
		t.Fatalf("expected leader after bootstrap")
	}
}

func TestDB_SetRoutesThroughRaftAndReadsLocal(t *testing.T) {
	d := openSingleNodeDB(t)

	if err := d.Set([]byte("codeq/tasks/abc"), []byte(`{"id":"abc"}`)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := d.Get([]byte("codeq/tasks/abc"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != `{"id":"abc"}` {
		t.Errorf("Get: want {\"id\":\"abc\"}, got %q", got)
	}
}

func TestDB_DeleteRoutesThroughRaft(t *testing.T) {
	d := openSingleNodeDB(t)
	_ = d.Set([]byte("codeq/tasks/x"), []byte("data"))

	if err := d.Delete([]byte("codeq/tasks/x")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := d.Get([]byte("codeq/tasks/x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-delete Get: want ErrNotFound, got %v", err)
	}
}

func TestDB_CommitBatchAtomicMultiKey(t *testing.T) {
	d := openSingleNodeDB(t)

	b := d.Batch()
	defer b.Close()
	_ = b.Set([]byte("codeq/tasks/1"), []byte("one"), nil)
	_ = b.Set([]byte("codeq/tasks/2"), []byte("two"), nil)
	_ = b.Delete([]byte("codeq/tasks/never"), nil)

	if err := d.CommitBatch(b); err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}
	for _, kv := range [][2]string{{"codeq/tasks/1", "one"}, {"codeq/tasks/2", "two"}} {
		v, err := d.Get([]byte(kv[0]))
		if err != nil {
			t.Errorf("Get %s: %v", kv[0], err)
			continue
		}
		if string(v) != kv[1] {
			t.Errorf("Get %s: want %q, got %q", kv[0], kv[1], v)
		}
	}
}

func TestDB_CommitBatch_EmptyIsNoop(t *testing.T) {
	d := openSingleNodeDB(t)
	b := d.Batch()
	defer b.Close()
	if err := d.CommitBatch(b); err != nil {
		t.Errorf("empty CommitBatch: want nil, got %v", err)
	}
}

func TestDB_NextSeqMonotonic(t *testing.T) {
	d := openSingleNodeDB(t)
	a := d.NextSeq()
	b := d.NextSeq()
	c := d.NextSeq()
	if !(b == a+1 && c == b+1) {
		t.Errorf("NextSeq not monotonic: %d %d %d", a, b, c)
	}
}

func TestDB_IsLeader_FollowerWritesReturnErrNotLeader(t *testing.T) {
	// We don't have a real follower in a single-node cluster, but we
	// can simulate the "not yet leader" state by creating the DB and
	// closing it (which transitions state to Shutdown, not Leader).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := Open(ctx, Config{
		Path:          t.TempDir(),
		SelfID:        "node-1",
		BindAddr:      "127.0.0.1:0",
		Bootstrap:     true,
		HeartbeatMS:   50,
		ElectionMS:    50,
		LeaderLeaseMS: 50,
		CommitMS:      10,
		ApplyTimeout:  1 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = d.WaitLeader(ctx)
	_ = d.Close()
	// After shutdown, IsLeader returns false.
	if d.IsLeader() {
		t.Errorf("post-Close IsLeader should be false")
	}
	if err := d.Set([]byte("k"), []byte("v")); !errors.Is(err, ErrNotLeader) {
		t.Errorf("post-Close Set: want ErrNotLeader, got %v", err)
	}
}

func TestDB_ReopenPreservesState(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First boot: bootstrap, write, close.
	d1, err := Open(ctx, Config{
		Path:          dir,
		SelfID:        "node-1",
		BindAddr:      "127.0.0.1:0",
		Bootstrap:     true,
		HeartbeatMS:   50,
		ElectionMS:    50,
		LeaderLeaseMS: 50,
		CommitMS:      10,
		ApplyTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := d1.WaitLeader(ctx); err != nil {
		t.Fatalf("WaitLeader: %v", err)
	}
	if err := d1.Set([]byte("codeq/tasks/persist"), []byte("yes")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second boot: reopen same dir without re-bootstrapping (HasState
	// returns true, BootstrapCluster is skipped).
	d2, err := Open(ctx, Config{
		Path:          dir,
		SelfID:        "node-1",
		BindAddr:      "127.0.0.1:0",
		Bootstrap:     true, // skipped because state exists
		HeartbeatMS:   50,
		ElectionMS:    50,
		LeaderLeaseMS: 50,
		CommitMS:      10,
		ApplyTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	if err := d2.WaitLeader(ctx); err != nil {
		t.Fatalf("WaitLeader after reopen: %v", err)
	}
	v, err := d2.Get([]byte("codeq/tasks/persist"))
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if string(v) != "yes" {
		t.Errorf("post-reopen value: want yes, got %q", v)
	}
}

func TestDB_IterReturnsLiveRange(t *testing.T) {
	d := openSingleNodeDB(t)
	for i, k := range []string{"codeq/q/a", "codeq/q/b", "codeq/q/c", "codeq/tasks/x"} {
		if err := d.Set([]byte(k), []byte{byte(i)}); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	iter, err := d.Iter([]byte("codeq/q/"), []byte("codeq/q0"))
	if err != nil {
		t.Fatalf("Iter: %v", err)
	}
	defer iter.Close()
	got := 0
	for ok := iter.First(); ok; ok = iter.Next() {
		got++
	}
	if got != 3 {
		t.Errorf("Iter codeq/q/ range: want 3, got %d", got)
	}
}

func TestDB_Health(t *testing.T) {
	d := openSingleNodeDB(t)
	if err := d.Health(context.Background()); err != nil {
		t.Errorf("Health: %v", err)
	}
}

func TestDB_SeqRecoveryOnReopen(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	d1, err := Open(ctx, Config{
		Path: dir, SelfID: "node-1", BindAddr: "127.0.0.1:0", Bootstrap: true,
		HeartbeatMS: 50, ElectionMS: 50, LeaderLeaseMS: 50, CommitMS: 10,
		ApplyTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = d1.WaitLeader(ctx)

	// Write a fake pending key with seq=42 so recoverSeq has something
	// to find on reopen. The pending key layout is:
	//   codeq/q/<cmd>/<tenant>/pending/<prio_be1>/<seq_be8>/<id>
	key := make([]byte, 0, 64)
	key = append(key, "codeq/q/CMD/T1/pending/"...)
	key = append(key, byte(0))           // prio
	key = append(key, byte('/'))         // sep
	for i := 7; i >= 0; i-- {            // seq=42 BE
		key = append(key, byte((uint64(42)>>(8*i))&0xff))
	}
	key = append(key, byte('/'))
	key = append(key, "id1"...)
	if err := d1.Set(key, []byte{}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	_ = d1.Close()

	// Reopen and check seq was recovered to 42.
	d2, err := Open(ctx, Config{
		Path: dir, SelfID: "node-1", BindAddr: "127.0.0.1:0", Bootstrap: true,
		HeartbeatMS: 50, ElectionMS: 50, LeaderLeaseMS: 50, CommitMS: 10,
		ApplyTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	next := d2.NextSeq()
	if next != 43 {
		t.Errorf("post-reopen NextSeq: want 43 (max+1), got %d", next)
	}
	// Quiet unused vars
	_ = pebbledb.Logger(nil)
}
