package raft

import (
	"fmt"
	"io"

	pebbledb "github.com/cockroachdb/pebble"
	hraft "github.com/hashicorp/raft"
)

// fsm implements hashicorp/raft.FSM over a Pebble store.
//
// Apply receives committed raft log entries (T5 wires this). Snapshot
// captures a consistent view of the codeq/ keyspace via pebble.Snapshot
// and streams it through the SnapshotSink using the format defined in
// snapshot.go. Restore reverses that — wipes the existing state and
// re-populates from the stream.
//
// The raft library guarantees Apply is serialized; Snapshot may run
// concurrently with Apply (raft creates the pebble.Snapshot before
// returning the FSMSnapshot, so the view is point-in-time).
type fsm struct {
	pebble *pebbledb.DB
}

func newFSM(pebble *pebbledb.DB) *fsm {
	return &fsm{pebble: pebble}
}

// Apply is called by raft once an entry is committed by the quorum.
// The Data field is a serialized batch this node turns into a Pebble
// commit. TODO(M1.T5).
func (f *fsm) Apply(log *hraft.Log) any {
	// Placeholder: return nil. T5 will deserialize log.Data and commit.
	return nil
}

// Snapshot returns a point-in-time snapshot of the FSM state. The
// pebble.Snapshot is created inside Snapshot() (synchronous w/ raft's
// FSM lock) and held by the returned fsmSnapshot until Persist or
// Release runs.
func (f *fsm) Snapshot() (hraft.FSMSnapshot, error) {
	snap := f.pebble.NewSnapshot()
	return &fsmSnapshot{snap: snap}, nil
}

// Restore loads FSM state from a snapshot stream. Called by raft on
// startup or after an install_snapshot RPC. The reader carries the
// format produced by writeSnapshot.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	if err := readSnapshot(rc, f.pebble); err != nil {
		return fmt.Errorf("fsm restore: %w", err)
	}
	return nil
}

// fsmSnapshot holds a pebble.Snapshot until raft calls Persist (write
// out to the sink) or Release (drop without writing).
type fsmSnapshot struct {
	snap *pebbledb.Snapshot
}

// Persist writes the snapshot bytes to sink. On success, sink.Close is
// called; on failure, sink.Cancel runs. Either way the pebble.Snapshot
// is closed.
func (s *fsmSnapshot) Persist(sink hraft.SnapshotSink) error {
	if err := writeSnapshot(sink, s.snap); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("fsm snapshot persist: %w", err)
	}
	return sink.Close()
}

// Release is called by raft whether Persist succeeded or not. Closing
// the pebble.Snapshot here (idempotent) reclaims its iterator
// resources.
func (s *fsmSnapshot) Release() {
	if s.snap != nil {
		_ = s.snap.Close()
		s.snap = nil
	}
}
