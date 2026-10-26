package raft

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	pebbledb "github.com/cockroachdb/pebble"
	hraft "github.com/hashicorp/raft"
)

// seedSnapshots publishes complete snapshots with the real hashicorp store
// under the layout openInternal uses, as an older release with retain=3 left
// them, and returns the store's directory.
func seedSnapshots(t *testing.T, path string, indexes ...uint64) string {
	t.Helper()
	store, err := newSnapshotStore(snapshotDirName(path), 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range indexes {
		sink, err := store.Create(hraft.SnapshotVersionMax, index, 1, hraft.Configuration{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = sink.Write(bytes.Repeat([]byte{7}, 64<<10)); err != nil {
			t.Fatal(err)
		}
		if err = sink.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(snapshotDirName(path), "snapshots")
}

func writeDir(t *testing.T, dir string, size int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotStateFile), bytes.Repeat([]byte{1}, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func listIndexes(t *testing.T, path string) []uint64 {
	t.Helper()
	store, err := newSnapshotStore(snapshotDirName(path), 3)
	if err != nil {
		t.Fatal(err)
	}
	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]uint64, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.Index)
	}
	return out
}

func TestReclaimSnapshotsBeforeOpenKeepsNewestAndDropsAbandoned(t *testing.T) {
	path := t.TempDir()
	dir := seedSnapshots(t, path, 10, 30, 20)
	abandoned := filepath.Join(dir, "1-40-1700000000000.tmp")
	writeDir(t, abandoned, 256<<10)
	foreign := filepath.Join(dir, "operator-notes.tmp")
	writeDir(t, foreign, 1)

	got, err := ReclaimSnapshotsBeforeOpen(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if idx := listIndexes(t, path); len(idx) != 1 || idx[0] != 30 {
		t.Fatalf("retained snapshots = %v, want only the newest (30)", idx)
	}
	if _, err = os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned snapshot survived: %v", err)
	}
	if _, err = os.Stat(foreign); err != nil {
		t.Fatalf("foreign directory removed: %v", err)
	}
	if got.Abandoned != 1 || got.FreedBytes < 2*(64<<10)+(256<<10) || got.RetainedBytes == 0 {
		t.Fatalf("reclaim = %+v", got)
	}

	// Idempotent: a second start finds nothing more to free.
	again, err := ReclaimSnapshotsBeforeOpen(path, 1)
	if err != nil || again.FreedBytes != 0 || again.Abandoned != 0 {
		t.Fatalf("second reclaim = %+v, %v", again, err)
	}
}

func TestReclaimSnapshotsBeforeOpenHonoursExplicitRetain(t *testing.T) {
	path := t.TempDir()
	seedSnapshots(t, path, 10, 20, 30)
	if _, err := ReclaimSnapshotsBeforeOpen(path, 2); err != nil {
		t.Fatal(err)
	}
	if idx := listIndexes(t, path); len(idx) != 2 || idx[0] != 30 || idx[1] != 20 {
		t.Fatalf("retained snapshots = %v, want [30 20]", idx)
	}
}

func TestReclaimSnapshotsBeforeOpenWithoutSnapshots(t *testing.T) {
	got, err := ReclaimSnapshotsBeforeOpen(t.TempDir(), 1)
	if err != nil || (got != SnapshotReclaim{}) {
		t.Fatalf("empty directory reclaim = %+v, %v", got, err)
	}
}

func TestReclaimSnapshotsBeforeOpenLeavesOpenStoreUntouched(t *testing.T) {
	path := t.TempDir()
	dir := seedSnapshots(t, path, 10, 20)
	abandoned := filepath.Join(dir, "1-30-1700000000000.tmp")
	writeDir(t, abandoned, 1024)
	db, err := pebbledb.Open(path, &pebbledb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err = ReclaimSnapshotsBeforeOpen(path, 1); err == nil {
		t.Fatal("reclaim succeeded while the data directory was open")
	}
	if idx := listIndexes(t, path); len(idx) != 2 {
		t.Fatalf("snapshots changed under an open store: %v", idx)
	}
	if _, err = os.Stat(abandoned); err != nil {
		t.Fatalf("in-progress snapshot of an open store removed: %v", err)
	}
}
