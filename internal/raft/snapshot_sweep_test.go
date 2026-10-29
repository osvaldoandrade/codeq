package raft

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	hraft "github.com/hashicorp/raft"
)

func newTestSweepingStore(t *testing.T) (*sweepingSnapshotStore, string) {
	t.Helper()
	base := filepath.Join(t.TempDir(), "snapshots")
	store, err := newSnapshotStore(base, 2)
	if err != nil {
		t.Fatalf("newSnapshotStore: %v", err)
	}
	sweeping, ok := store.(*sweepingSnapshotStore)
	if !ok {
		t.Fatalf("newSnapshotStore returned %T, want *sweepingSnapshotStore", store)
	}
	return sweeping, filepath.Join(base, "snapshots")
}

func mustDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "state.bin"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestSweepingStoreRemovesAbandonedSnapshotAfterNextSnapshot(t *testing.T) {
	store, dir := newTestSweepingStore(t)
	abandoned := filepath.Join(dir, "3-90-1700000000000.tmp")
	foreign := filepath.Join(dir, "notes.tmp")
	mustDir(t, abandoned)
	mustDir(t, foreign)

	sink, err := store.Create(hraft.SnapshotVersionMax, 100, 3, hraft.Configuration{}, 1, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := sink.Write([]byte("state")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !exists(abandoned) {
		t.Fatal("abandoned snapshot removed before the new snapshot finished")
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if exists(abandoned) {
		t.Fatal("abandoned snapshot survived the next completed snapshot")
	}
	if !exists(foreign) {
		t.Fatal("a folder that is not a snapshot was removed")
	}
	if !exists(filepath.Join(dir, sink.ID())) {
		t.Fatal("the completed snapshot itself was removed")
	}
	if metas, err := store.List(); err != nil || len(metas) != 1 || metas[0].ID != sink.ID() {
		t.Fatalf("List() = %v, %v; want the one completed snapshot", metas, err)
	}
}

func TestSweepingStoreKeepsSnapshotsStillBeingWritten(t *testing.T) {
	store, dir := newTestSweepingStore(t)
	writing, err := store.Create(hraft.SnapshotVersionMax, 200, 4, hraft.Configuration{}, 1, nil)
	if err != nil {
		t.Fatalf("Create writing: %v", err)
	}
	other, err := store.Create(hraft.SnapshotVersionMax, 201, 4, hraft.Configuration{}, 1, nil)
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}
	if err := other.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !exists(filepath.Join(dir, writing.ID()+snapshotTmpSuffix)) {
		t.Fatal("a snapshot still being written was swept")
	}
	if _, err := writing.Write([]byte("state")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := writing.Close(); err != nil {
		t.Fatalf("Close after a concurrent sweep: %v", err)
	}
	if !exists(filepath.Join(dir, writing.ID())) {
		t.Fatal("the snapshot written during a sweep did not complete")
	}
}

// failingStore stands in for FileSnapshotStore failing the way ENOSPC makes
// it fail: the "<id>.tmp" folder is left behind by Create, or by a sink whose
// Close cannot finish.
type failingStore struct {
	hraft.SnapshotStore
	dir         string
	failCreate  bool
	createdSink *failingSink
}

type failingSink struct {
	id string
}

func (s *failingSink) ID() string                  { return s.id }
func (s *failingSink) Write(p []byte) (int, error) { return len(p), nil }
func (s *failingSink) Close() error                { return errors.New("no space left on device") }
func (s *failingSink) Cancel() error               { return errors.New("no space left on device") }

func (s *failingStore) Create(hraft.SnapshotVersion, uint64, uint64, hraft.Configuration, uint64, hraft.Transport) (hraft.SnapshotSink, error) {
	id := "5-500-1700000000500"
	if err := os.MkdirAll(filepath.Join(s.dir, id+snapshotTmpSuffix), 0o700); err != nil {
		return nil, err
	}
	if s.failCreate {
		return nil, errors.New("no space left on device")
	}
	s.createdSink = &failingSink{id: id}
	return s.createdSink, nil
}

func TestSweepingStoreRemovesWhatAFailedCreateLeaves(t *testing.T) {
	dir := t.TempDir()
	store := newSweepingSnapshotStore(&failingStore{dir: dir, failCreate: true}, dir, io.Discard)
	if _, err := store.Create(hraft.SnapshotVersionMax, 500, 5, hraft.Configuration{}, 1, nil); err == nil {
		t.Fatal("Create should fail")
	}
	if exists(filepath.Join(dir, "5-500-1700000000500"+snapshotTmpSuffix)) {
		t.Fatal("the folder left by a failed Create was not swept")
	}
}

func TestSweepingStoreRemovesWhatAFailedCloseLeaves(t *testing.T) {
	for _, finish := range []string{"Close", "Cancel"} {
		t.Run(finish, func(t *testing.T) {
			dir := t.TempDir()
			store := newSweepingSnapshotStore(&failingStore{dir: dir}, dir, io.Discard)
			sink, err := store.Create(hraft.SnapshotVersionMax, 500, 5, hraft.Configuration{}, 1, nil)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			tmp := filepath.Join(dir, sink.ID()+snapshotTmpSuffix)
			if !exists(tmp) {
				t.Fatal("open snapshot swept")
			}
			if finish == "Close" {
				err = sink.Close()
			} else {
				err = sink.Cancel()
			}
			if err == nil {
				t.Fatalf("%s should return the store's error", finish)
			}
			if exists(tmp) {
				t.Fatalf("the folder left by a failed %s was not swept", finish)
			}
			if len(store.open) != 0 {
				t.Fatalf("snapshot still tracked as open: %v", store.open)
			}
		})
	}
}

// Snapshots taken concurrently (local snapshot and InstallSnapshot both
// Create through the store) never sweep each other. Run with -race.
func TestSweepingStoreConcurrentSnapshotsNeverSweepEachOther(t *testing.T) {
	const writers = 8
	base := filepath.Join(t.TempDir(), "snapshots")
	store, err := newSnapshotStore(base, writers)
	if err != nil {
		t.Fatalf("newSnapshotStore: %v", err)
	}
	dir := filepath.Join(base, "snapshots")
	var wg sync.WaitGroup
	ids := make([]string, writers)
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sink, err := store.Create(hraft.SnapshotVersionMax, uint64(100+i), 7, hraft.Configuration{}, 1, nil)
			if err != nil {
				errs <- err
				return
			}
			ids[i] = sink.ID()
			if _, err := sink.Write([]byte("state")); err != nil {
				errs <- err
				return
			}
			if i%2 == 0 {
				errs <- sink.Close()
				return
			}
			_ = sink.Cancel()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent snapshot failed: %v", err)
		}
	}
	for i, id := range ids {
		if i%2 == 0 && !exists(filepath.Join(dir, id)) {
			t.Fatalf("completed snapshot %s missing", id)
		}
		if exists(filepath.Join(dir, id+snapshotTmpSuffix)) {
			t.Fatalf("temp folder %s left behind", id)
		}
	}
}
