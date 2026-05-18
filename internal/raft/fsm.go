package raft

import (
	"fmt"
	"io"

	pebbledb "github.com/cockroachdb/pebble"
	hraft "github.com/hashicorp/raft"
)

// fsm implements hashicorp/raft.FSM over a Pebble store.
//
// Apply receives committed raft log entries. The Data field is the
// output of pebble.Batch.Repr() produced by the leader (see db.go
// CommitBatch in T6). FSM.Apply turns those bytes back into a Pebble
// batch via SetRepr and commits it. Every replica in the cluster runs
// the same Apply on the same input, so all Pebble stores converge to
// the same state.
//
// Snapshot captures a consistent view of the codeq/ keyspace via
// pebble.NewSnapshot and streams it through the SnapshotSink using the
// format defined in snapshot.go. Restore reverses that — wipes the
// existing codeq/ range and re-populates from the stream.
//
// raft guarantees Apply is serialized; Snapshot may run concurrently
// with Apply (raft creates the pebble.Snapshot inside our Snapshot()
// before returning the FSMSnapshot, so the view is point-in-time).
type fsm struct {
	pebble *pebbledb.DB
}

func newFSM(pebble *pebbledb.DB) *fsm {
	return &fsm{pebble: pebble}
}

// Apply turns a committed raft log entry into a Pebble batch commit.
// Returns nil on success or an error explaining the failure. The caller
// retrieves the return value via raft.ApplyFuture.Response().
//
// Non-LogCommand entries (LogNoop, LogBarrier, LogConfiguration) and
// empty payloads are no-ops — raft expects FSM.Apply to be defensive
// against these even though the runFSM dispatcher filters most of them.
func (f *fsm) Apply(log *hraft.Log) any {
	if log == nil || log.Type != hraft.LogCommand || len(log.Data) == 0 {
		return nil
	}
	// Copy the slice before handing it to pebble — Batch.SetRepr takes
	// ownership of its argument and raft's log buffers are reused on
	// the next dispatch.
	repr := make([]byte, len(log.Data))
	copy(repr, log.Data)

	batch := f.pebble.NewBatch()
	defer batch.Close()
	if err := batch.SetRepr(repr); err != nil {
		return fmt.Errorf("fsm apply: SetRepr: %w", err)
	}
	if err := batch.Commit(pebbledb.NoSync); err != nil {
		return fmt.Errorf("fsm apply: commit: %w", err)
	}
	return nil
}

// Snapshot returns a point-in-time snapshot of the FSM state. The
// pebble.Snapshot is created inside Snapshot() (synchronous with the
// raft FSM lock) and held by the returned fsmSnapshot until Persist or
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
// is closed via Release.
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
