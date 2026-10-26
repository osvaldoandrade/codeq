package raft

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/pebble/vfs"
	hraft "github.com/hashicorp/raft"
)

// SnapshotReclaim reports what ReclaimSnapshotsBeforeOpen removed.
type SnapshotReclaim struct {
	// Abandoned is the number of in-progress (.tmp) snapshot directories
	// left by a process that died while writing a snapshot.
	Abandoned int
	// FreedBytes is the space released, measured before and after.
	FreedBytes uint64
	// RetainedBytes is what the snapshot directory still holds.
	RetainedBytes uint64
}

// ReclaimSnapshotsBeforeOpen frees Raft snapshot space on a voter's data
// directory before Pebble and Raft open it. It is the startup half of the
// retention hashicorp/raft applies after every new snapshot:
//
//   - in-progress "<id>.tmp" directories are removed. hashicorp/raft never
//     lists, restores, sends or reaps them, so one left by a process that
//     died mid-snapshot (ENOSPC, OOM, eviction) stays on disk forever;
//   - complete snapshots beyond retain are reaped with hashicorp's own
//     ordering (newest term/index first), so the newest complete snapshot
//     always stays.
//
// Without it a volume filled under an older, larger retention (or by an
// abandoned snapshot) cannot open Pebble at all: WAL replay must flush a
// memtable before Raft ever runs, so the configured retention is never
// reached. path is the shard's Pebble directory (raft Config.Path). The
// Pebble directory lock is held for the duration, so a store still open by
// another process is left untouched (an error is returned). A missing
// snapshot directory is not an error. retain < 1 selects the default.
func ReclaimSnapshotsBeforeOpen(path string, retain int) (SnapshotReclaim, error) {
	var out SnapshotReclaim
	base := snapshotDirName(path)
	dir := filepath.Join(base, "snapshots")
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return out, fmt.Errorf("raft: stat snapshot dir: %w", err)
	}
	lock, err := vfs.Default.Lock(filepath.Join(path, "LOCK"))
	if err != nil {
		return out, fmt.Errorf("raft: data directory is in use, snapshots left untouched: %w", err)
	}
	defer lock.Close()

	before, err := MeasureSnapshotDir(dir)
	if err != nil {
		return out, err
	}
	out.Abandoned, err = removeAbandonedSnapshots(dir)
	if err == nil {
		err = reapCompleteSnapshots(base, retain)
	}
	after, merr := MeasureSnapshotDir(dir)
	if merr == nil {
		out.RetainedBytes = after.OnDiskBytes
		if before.OnDiskBytes > after.OnDiskBytes {
			out.FreedBytes = before.OnDiskBytes - after.OnDiskBytes
		}
	}
	return out, errors.Join(err, merr)
}

func removeAbandonedSnapshots(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("raft: read snapshot dir: %w", err)
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), snapshotTmpSuffix) {
			continue
		}
		if _, ok := parseSnapshotID(strings.TrimSuffix(e.Name(), snapshotTmpSuffix)); !ok {
			continue // not a hashicorp snapshot name: never ours to delete
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return removed, fmt.Errorf("raft: remove abandoned snapshot %s: %w", e.Name(), err)
		}
		removed++
	}
	return removed, nil
}

func reapCompleteSnapshots(base string, retain int) error {
	if retain < 1 {
		retain = DefaultSnapshotRetain
	}
	store, err := hraft.NewFileSnapshotStore(base, retain, io.Discard)
	if err != nil {
		return fmt.Errorf("raft: open snapshot store: %w", err)
	}
	if err := store.ReapSnapshots(); err != nil {
		return fmt.Errorf("raft: reap snapshots: %w", err)
	}
	return nil
}
