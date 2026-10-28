package raft

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	hraft "github.com/hashicorp/raft"
)

// sweepingSnapshotStore removes snapshot directories abandoned while the
// voter keeps running. hashicorp/raft's FileSnapshotStore writes every
// snapshot into "<id>.tmp" and renames it on Close, but leaves the folder
// behind when Create, Close or Cancel itself fails (ENOSPC while writing
// meta.json or flushing state.bin), and never lists, reaps or reuses it.
// ReclaimSnapshotsBeforeOpen only runs at startup, so a running voter kept
// every such copy (hundreds of MiB each) until it restarted.
//
// Every sink created through this store is tracked while it is open. After
// each Create failure and each sink Close or Cancel, any "<id>.tmp" folder
// that no open sink owns is removed. Create and the sweep share one lock,
// so a folder being created is never mistaken for an abandoned one.
type sweepingSnapshotStore struct {
	hraft.SnapshotStore
	dir string // the FileSnapshotStore's own "snapshots" directory
	log io.Writer

	mu   sync.Mutex
	open map[string]struct{}
}

func newSweepingSnapshotStore(inner hraft.SnapshotStore, dir string, logOut io.Writer) *sweepingSnapshotStore {
	if logOut == nil {
		logOut = io.Discard
	}
	return &sweepingSnapshotStore{SnapshotStore: inner, dir: dir, log: logOut, open: make(map[string]struct{})}
}

// Create opens a snapshot sink through the wrapped store and tracks it until
// the sink is closed or canceled. A failed Create sweeps what it left.
func (s *sweepingSnapshotStore) Create(version hraft.SnapshotVersion, index, term uint64,
	configuration hraft.Configuration, configurationIndex uint64, trans hraft.Transport,
) (hraft.SnapshotSink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sink, err := s.SnapshotStore.Create(version, index, term, configuration, configurationIndex, trans)
	if err != nil {
		s.sweepLocked()
		return nil, err
	}
	s.open[sink.ID()] = struct{}{}
	return &sweepingSnapshotSink{SnapshotSink: sink, store: s}, nil
}

func (s *sweepingSnapshotStore) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, id)
	s.sweepLocked()
}

func (s *sweepingSnapshotStore) sweepLocked() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		fmt.Fprintf(s.log, "raft: snapshot sweep: read %s: %v\n", s.dir, err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasSuffix(name, snapshotTmpSuffix) {
			continue
		}
		id := strings.TrimSuffix(name, snapshotTmpSuffix)
		if _, ok := parseSnapshotID(id); !ok {
			continue // not a hashicorp snapshot name: never ours to delete
		}
		if _, busy := s.open[id]; busy {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.dir, name)); err != nil {
			fmt.Fprintf(s.log, "raft: snapshot sweep: remove abandoned snapshot %s: %v\n", name, err)
			continue
		}
		fmt.Fprintf(s.log, "raft: snapshot sweep: removed abandoned snapshot %s\n", name)
	}
}

// sweepingSnapshotSink releases its snapshot ID exactly once, after the
// wrapped sink has finished (renamed into place, or failed).
type sweepingSnapshotSink struct {
	hraft.SnapshotSink
	store *sweepingSnapshotStore
	once  sync.Once
}

// Close finishes the snapshot, then releases it and sweeps abandoned ones.
func (k *sweepingSnapshotSink) Close() error {
	err := k.SnapshotSink.Close()
	k.once.Do(func() { k.store.release(k.ID()) })
	return err
}

// Cancel abandons the snapshot, then releases it and sweeps abandoned ones,
// including this one when the wrapped Cancel could not remove it.
func (k *sweepingSnapshotSink) Cancel() error {
	err := k.SnapshotSink.Cancel()
	k.once.Do(func() { k.store.release(k.ID()) })
	return err
}
