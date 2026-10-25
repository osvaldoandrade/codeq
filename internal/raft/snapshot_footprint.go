package raft

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SmallVolumeMaxBytes is the largest data volume treated as "small" when
// raft.snapshotRetain is left on auto: every retained snapshot is a full
// copy of the queue state, and on a 1 GiB voter volume the second copy is
// the difference between a snapshot fitting and ENOSPC.
const SmallVolumeMaxBytes = uint64(1 << 30)

// SmallVolumeSnapshotRetain is the auto retention on small volumes.
const SmallVolumeSnapshotRetain = 1

// AutoSnapshotRetain picks the snapshot retention when none is configured:
// 1 on volumes of at most SmallVolumeMaxBytes, DefaultSnapshotRetain on
// larger or unknown (0) volumes. hashicorp/raft writes a new snapshot fully
// before reaping old ones, so retain=1 still peaks at two copies on disk and
// never deletes the only complete snapshot.
func AutoSnapshotRetain(volumeTotalBytes uint64) int {
	if volumeTotalBytes > 0 && volumeTotalBytes <= SmallVolumeMaxBytes {
		return SmallVolumeSnapshotRetain
	}
	return DefaultSnapshotRetain
}

// SnapshotDiskUsage describes one FileSnapshotStore directory.
type SnapshotDiskUsage struct {
	// LatestBytes is the state size of the newest complete snapshot
	// (0 when none exists yet).
	LatestBytes uint64
	// OnDiskBytes is every byte under the directory, including retained
	// older snapshots and an in-progress (.tmp) one.
	OnDiskBytes uint64
}

const (
	snapshotTmpSuffix = ".tmp"
	snapshotStateFile = "state.bin"
)

// snapshotDirName returns the FileSnapshotStore base for a raft path.
func snapshotDirName(path string) string { return path + "/snapshots" }

// MeasureSnapshotStore sizes the snapshots of a FileSnapshotStore created
// with base (hashicorp nests them in base/snapshots).
func MeasureSnapshotStore(base string) (SnapshotDiskUsage, error) {
	return MeasureSnapshotDir(filepath.Join(base, "snapshots"))
}

// MeasureSnapshotDir sizes the directory that holds the snapshot
// directories of a FileSnapshotStore without going
// through hashicorp's List (which logs a warning per in-progress snapshot
// and reads every meta.json). Snapshot directories are named
// "<term>-<index>-<msec>"; the newest complete one is the highest tuple,
// matching hashicorp's ordering. A missing directory is empty, not an error.
func MeasureSnapshotDir(dir string) (SnapshotDiskUsage, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return SnapshotDiskUsage{}, nil
	}
	if err != nil {
		return SnapshotDiskUsage{}, fmt.Errorf("raft: read snapshot dir: %w", err)
	}
	var (
		out    SnapshotDiskUsage
		newest snapshotID
		found  bool
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		size, err := dirFileBytes(sub)
		if err != nil {
			return SnapshotDiskUsage{}, err
		}
		out.OnDiskBytes += size
		id, stateBytes, ok := completeSnapshot(sub, e.Name())
		if ok && (!found || newest.less(id)) {
			newest, found, out.LatestBytes = id, true, stateBytes
		}
	}
	return out, nil
}

// completeSnapshot reports the id and state size of a published snapshot
// directory; in-progress (.tmp), foreign or incomplete ones are skipped.
func completeSnapshot(dir, name string) (snapshotID, uint64, bool) {
	if strings.HasSuffix(name, snapshotTmpSuffix) {
		return snapshotID{}, 0, false
	}
	id, ok := parseSnapshotID(name)
	if !ok {
		return snapshotID{}, 0, false
	}
	info, err := os.Stat(filepath.Join(dir, snapshotStateFile))
	if err != nil {
		return snapshotID{}, 0, false
	}
	return id, uint64(max(info.Size(), 0)), true // #nosec G115 -- clamped non-negative
}

// dirFileBytes sums the regular files directly inside dir (a snapshot
// directory holds only meta.json and state.bin).
func dirFileBytes(dir string) (uint64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil // reaped between the two reads
	}
	if err != nil {
		return 0, fmt.Errorf("raft: read snapshot %s: %w", filepath.Base(dir), err)
	}
	var total uint64
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed concurrently
		}
		total += uint64(max(info.Size(), 0)) // #nosec G115 -- clamped non-negative
	}
	return total, nil
}

type snapshotID struct{ term, index, msec uint64 }

func (a snapshotID) less(b snapshotID) bool {
	if a.term != b.term {
		return a.term < b.term
	}
	if a.index != b.index {
		return a.index < b.index
	}
	return a.msec < b.msec
}

func parseSnapshotID(name string) (snapshotID, bool) {
	parts := strings.Split(name, "-")
	if len(parts) != 3 {
		return snapshotID{}, false
	}
	var vals [3]uint64
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return snapshotID{}, false
		}
		vals[i] = v
	}
	return snapshotID{term: vals[0], index: vals[1], msec: vals[2]}, true
}

// SnapshotRetain reports the retention this node runs with.
func (d *DB) SnapshotRetain() int { return d.cfg.snapshotRetain() }

// SnapshotDiskUsage measures this shard's snapshot directory.
func (d *DB) SnapshotDiskUsage() (SnapshotDiskUsage, error) {
	return MeasureSnapshotStore(snapshotDirName(d.cfg.Path))
}

// LiveStateBytes estimates the on-disk size of the replicated FSM range
// (the keys a snapshot copies). Pebble reports compressed sstable bytes, so
// it is a lower bound on the uncompressed snapshot.
func (d *DB) LiveStateBytes() (uint64, error) {
	n, err := d.pebble.EstimateDiskUsage([]byte(snapshotStatePrefix), []byte(snapshotStateEnd))
	if err != nil {
		return 0, fmt.Errorf("raft: estimate state size: %w", err)
	}
	return n, nil
}
