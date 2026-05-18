package raft

import (
	"io"

	hraft "github.com/hashicorp/raft"

	pebbledb "github.com/cockroachdb/pebble"
)

// fsm implements hashicorp/raft.FSM over a Pebble store. Apply receives
// committed raft log entries and applies them to the local Pebble store
// as a single atomic batch.
//
// M1 status: skeleton. Apply is stubbed (T5). Snapshot/Restore stubbed
// (T4).
type fsm struct {
	pebble *pebbledb.DB
}

func newFSM(pebble *pebbledb.DB) *fsm {
	return &fsm{pebble: pebble}
}

// Apply is called by raft once an entry is committed by the quorum.
// The Data field is a serialized pebble.Batch representation that this
// node turns into a real Pebble batch and commits atomically.
//
// TODO(M1.T5): implement.
func (f *fsm) Apply(log *hraft.Log) interface{} {
	// Placeholder: return nil. T5 will deserialize log.Data and commit.
	return nil
}

// Snapshot returns a point-in-time snapshot of the FSM state. Uses
// pebble.Checkpoint() under the hood (T4).
func (f *fsm) Snapshot() (hraft.FSMSnapshot, error) {
	// TODO(M1.T4): pebble.Checkpoint to a temp dir, return a snapshot
	// type whose Persist tars the dir into the sink.
	return &snapshotStub{}, nil
}

// Restore loads FSM state from a snapshot. Called by raft on startup
// or after install_snapshot RPC.
//
// TODO(M1.T4): untar the reader into a fresh Pebble dir, swap the
// active store.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	return nil
}

// snapshotStub is the M1 placeholder hraft.FSMSnapshot. T4 replaces it
// with a real Pebble Checkpoint based implementation.
type snapshotStub struct{}

func (s *snapshotStub) Persist(sink hraft.SnapshotSink) error {
	return sink.Close()
}

func (s *snapshotStub) Release() {}
