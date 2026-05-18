package raft

import (
	"bytes"
	"strings"
	"testing"

	pebbledb "github.com/cockroachdb/pebble"
	hraft "github.com/hashicorp/raft"
)

func openFSMTestDB(t *testing.T) (*pebbledb.DB, *fsm) {
	t.Helper()
	dir := t.TempDir()
	db, err := pebbledb.Open(dir, &pebbledb.Options{})
	if err != nil {
		t.Fatalf("pebble open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, newFSM(db)
}

// buildBatchRepr returns the wire bytes for a batch the leader would
// build and hand off to raft via raft.Apply().
func buildBatchRepr(t *testing.T, db *pebbledb.DB, build func(*pebbledb.Batch)) []byte {
	t.Helper()
	batch := db.NewBatch()
	defer batch.Close()
	build(batch)
	repr := batch.Repr()
	out := make([]byte, len(repr))
	copy(out, repr)
	return out
}

func TestFSM_Apply_SetWritesKey(t *testing.T) {
	db, f := openFSMTestDB(t)
	repr := buildBatchRepr(t, db, func(b *pebbledb.Batch) {
		_ = b.Set([]byte("codeq/tasks/abc"), []byte(`{"id":"abc"}`), nil)
	})

	res := f.Apply(&hraft.Log{Type: hraft.LogCommand, Data: repr})
	if res != nil {
		t.Fatalf("Apply returned %v", res)
	}
	v, closer, err := db.Get([]byte("codeq/tasks/abc"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer closer.Close()
	if string(v) != `{"id":"abc"}` {
		t.Errorf("value: want {\"id\":\"abc\"}, got %q", v)
	}
}

func TestFSM_Apply_DeleteRemovesKey(t *testing.T) {
	db, f := openFSMTestDB(t)
	// Seed a key directly so the deletion has something to remove.
	if err := db.Set([]byte("codeq/tasks/x"), []byte("data"), pebbledb.NoSync); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repr := buildBatchRepr(t, db, func(b *pebbledb.Batch) {
		_ = b.Delete([]byte("codeq/tasks/x"), nil)
	})

	if res := f.Apply(&hraft.Log{Type: hraft.LogCommand, Data: repr}); res != nil {
		t.Fatalf("Apply returned %v", res)
	}
	_, _, err := db.Get([]byte("codeq/tasks/x"))
	if err != pebbledb.ErrNotFound {
		t.Errorf("post-delete Get: want ErrNotFound, got %v", err)
	}
}

func TestFSM_Apply_MixedSetDelete_Atomic(t *testing.T) {
	db, f := openFSMTestDB(t)
	if err := db.Set([]byte("codeq/q/old"), []byte("oldval"), pebbledb.NoSync); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repr := buildBatchRepr(t, db, func(b *pebbledb.Batch) {
		// Pattern from Claim: delete pending, set inprog + task body in
		// the same atomic batch.
		_ = b.Delete([]byte("codeq/q/old"), nil)
		_ = b.Set([]byte("codeq/q/new"), []byte("newval"), nil)
		_ = b.Set([]byte("codeq/tasks/x"), []byte(`{"status":"IN_PROGRESS"}`), nil)
	})
	if res := f.Apply(&hraft.Log{Type: hraft.LogCommand, Data: repr}); res != nil {
		t.Fatalf("Apply returned %v", res)
	}

	// Old key gone.
	if _, _, err := db.Get([]byte("codeq/q/old")); err != pebbledb.ErrNotFound {
		t.Errorf("old key: want ErrNotFound, got %v", err)
	}
	// New key + task body present.
	for _, k := range []string{"codeq/q/new", "codeq/tasks/x"} {
		_, closer, err := db.Get([]byte(k))
		if err != nil {
			t.Errorf("post-Apply Get %s: %v", k, err)
			continue
		}
		closer.Close()
	}
}

func TestFSM_Apply_NonLogCommandIsNoop(t *testing.T) {
	db, f := openFSMTestDB(t)
	repr := buildBatchRepr(t, db, func(b *pebbledb.Batch) {
		_ = b.Set([]byte("codeq/tasks/never"), []byte("should-not-apply"), nil)
	})
	// LogNoop carries Data but FSM.Apply should ignore it.
	if res := f.Apply(&hraft.Log{Type: hraft.LogNoop, Data: repr}); res != nil {
		t.Errorf("LogNoop Apply: want nil, got %v", res)
	}
	if _, _, err := db.Get([]byte("codeq/tasks/never")); err != pebbledb.ErrNotFound {
		t.Errorf("LogNoop should not have applied; got err=%v", err)
	}
}

func TestFSM_Apply_EmptyDataIsNoop(t *testing.T) {
	_, f := openFSMTestDB(t)
	if res := f.Apply(&hraft.Log{Type: hraft.LogCommand, Data: nil}); res != nil {
		t.Errorf("empty Data: want nil, got %v", res)
	}
	if res := f.Apply(&hraft.Log{Type: hraft.LogCommand, Data: []byte{}}); res != nil {
		t.Errorf("empty Data slice: want nil, got %v", res)
	}
}

func TestFSM_Apply_CorruptDataReturnsError(t *testing.T) {
	_, f := openFSMTestDB(t)
	// Way too short — pebble batch header is 12 bytes.
	res := f.Apply(&hraft.Log{Type: hraft.LogCommand, Data: []byte{0x01, 0x02}})
	err, ok := res.(error)
	if !ok || err == nil {
		t.Fatalf("Apply on corrupt data: want error, got %v (type %T)", res, res)
	}
	if !strings.Contains(err.Error(), "SetRepr") {
		t.Errorf("error should mention SetRepr step, got %v", err)
	}
}

// TestFSM_Apply_ReplicationToSiblings simulates two replicas applying
// the same batch and verifies they end up with identical state. This is
// the property raft needs: deterministic application of the log.
func TestFSM_Apply_ReplicationToSiblings(t *testing.T) {
	dbA, fA := openFSMTestDB(t)
	dbB, fB := openFSMTestDB(t)

	repr := buildBatchRepr(t, dbA, func(b *pebbledb.Batch) {
		_ = b.Set([]byte("codeq/tasks/1"), []byte("one"), nil)
		_ = b.Set([]byte("codeq/tasks/2"), []byte("two"), nil)
		_ = b.Delete([]byte("codeq/never-existed"), nil)
	})

	if res := fA.Apply(&hraft.Log{Type: hraft.LogCommand, Data: repr}); res != nil {
		t.Fatalf("fA Apply: %v", res)
	}
	// Copy the bytes; raft would send these over the network so the
	// receiver doesn't share memory with the sender.
	reprCopy := append([]byte(nil), repr...)
	if res := fB.Apply(&hraft.Log{Type: hraft.LogCommand, Data: reprCopy}); res != nil {
		t.Fatalf("fB Apply: %v", res)
	}

	for _, kv := range [][2]string{
		{"codeq/tasks/1", "one"},
		{"codeq/tasks/2", "two"},
	} {
		va, ca, errA := dbA.Get([]byte(kv[0]))
		if errA != nil {
			t.Errorf("dbA Get %s: %v", kv[0], errA)
			continue
		}
		vb, cb, errB := dbB.Get([]byte(kv[0]))
		if errB != nil {
			t.Errorf("dbB Get %s: %v", kv[0], errB)
			ca.Close()
			continue
		}
		if !bytes.Equal(va, vb) || string(va) != kv[1] {
			t.Errorf("divergence on %s: A=%q B=%q want %q", kv[0], va, vb, kv[1])
		}
		ca.Close()
		cb.Close()
	}
}

// TestFSM_Apply_SnapshotReflectsAppliedData wires Apply → Snapshot →
// Restore end-to-end. A replica that misses entries and catches up via
// snapshot should land in the same state as one that applied every
// entry.
func TestFSM_Apply_SnapshotReflectsAppliedData(t *testing.T) {
	dbA, fA := openFSMTestDB(t)
	dbB, _ := openFSMTestDB(t) // never sees the Apply, will Restore from snapshot

	repr := buildBatchRepr(t, dbA, func(b *pebbledb.Batch) {
		_ = b.Set([]byte("codeq/tasks/applied"), []byte("yes"), nil)
	})
	if res := fA.Apply(&hraft.Log{Type: hraft.LogCommand, Data: repr}); res != nil {
		t.Fatalf("Apply: %v", res)
	}

	snap, err := fA.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer snap.Release()

	var buf bytes.Buffer
	if err := writeSnapshot(&buf, snap.(*fsmSnapshot).snap); err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}

	// Restore into dbB and verify the key shows up.
	if err := readSnapshot(&buf, dbB); err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	v, closer, err := dbB.Get([]byte("codeq/tasks/applied"))
	if err != nil {
		t.Fatalf("dbB Get: %v", err)
	}
	defer closer.Close()
	if string(v) != "yes" {
		t.Errorf("dbB value: want yes, got %q", v)
	}
}
