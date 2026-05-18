// Package raft provides a Pebble-backed, Raft-replicated KV with the
// same surface as internal/repository/pebble.DB. Writes flow through
// raft.Apply and land in the local Pebble store via the FSM; reads are
// local (and may be stale on followers).
//
// M1 status: skeleton — Open/Close + read pass-through wired; write path
// is stubbed and will land in T6. See /home/ova/.claude/plans/woolly-stargazing-orbit.md.
package raft

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	hraft "github.com/hashicorp/raft"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
)

// Config configures the raft-pebble DB. PeerAddrs lists every peer in
// the cluster (including self) by stable ID and bind address. SelfID
// must appear in PeerAddrs. Bootstrap=true makes this node attempt to
// bootstrap a fresh cluster; only the first node started in a new
// deployment should do this.
type Config struct {
	Path           string
	FsyncOnCommit  bool
	SelfID         string
	BindAddr       string
	Bootstrap      bool
	PeerAddrs      map[string]string // id → bind addr
	HeartbeatMS    int               // raft heartbeat (default 1000)
	ElectionMS     int               // raft election (default 1000)
	LeaderLeaseMS  int               // raft leader lease (default 500)
	SnapshotEntries uint64           // log entries before snapshot (default 8192)
}

// DB is a Pebble store with raft replication. Reads are local; writes
// go through the FSM. The API mirrors internal/repository/pebble.DB so
// the rest of codeq can swap one for the other.
type DB struct {
	pebble *pebbledb.DB
	raft   *hraft.Raft
	fsm    *fsm
	cfg    Config

	seq atomic.Uint64

	leaderCh chan bool
	stopCh   chan struct{}
	stopped  chan struct{}

	mu sync.Mutex

	// Leases mirrors the volatile lease table on internal/repository/pebble.
	// Wiring it here keeps the wrapper API identical for the repository
	// layer. The table is per-node (volatile) and rebuilt on leadership
	// change (see leader.go).
	Leases *LeaseTable
}

// Open creates or opens a raft-pebble DB. The Pebble store lives under
// cfg.Path/state/, raft log under cfg.Path/raft/log, stable store under
// cfg.Path/raft/stable, snapshots under cfg.Path/raft/snap.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("raft: Path is required")
	}
	if cfg.SelfID == "" {
		return nil, fmt.Errorf("raft: SelfID is required")
	}
	if cfg.BindAddr == "" {
		return nil, fmt.Errorf("raft: BindAddr is required")
	}

	pOpts := &pebbledb.Options{
		Cache: pebbledb.NewCache(256 << 20),
	}
	for i := range pOpts.Levels {
		pOpts.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
		pOpts.Levels[i].FilterType = pebbledb.TableFilter
	}
	statePath := cfg.Path + "/state"
	pdb, err := pebbledb.Open(statePath, pOpts)
	if err != nil {
		return nil, fmt.Errorf("pebble open %s: %w", statePath, err)
	}

	d := &DB{
		pebble:   pdb,
		cfg:      cfg,
		leaderCh: make(chan bool, 8),
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
		Leases:   newLeaseTable(),
	}

	// TODO(M1.T2-T5): wire LogStore/StableStore/SnapshotStore + FSM +
	// Transport, then call hraft.NewRaft. For now we leave d.raft nil so
	// callers can build/test the surrounding code; IsLeader returns true
	// (single-node degenerate behavior) until raft is wired.
	d.fsm = newFSM(pdb)

	if err := d.recoverSeq(); err != nil {
		_ = pdb.Close()
		return nil, fmt.Errorf("recover seq: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error {
	if d == nil {
		return nil
	}
	close(d.stopCh)
	select {
	case <-d.stopped:
	default:
	}
	if d.raft != nil {
		f := d.raft.Shutdown()
		if err := f.Error(); err != nil {
			return fmt.Errorf("raft shutdown: %w", err)
		}
	}
	if d.pebble != nil {
		return d.pebble.Close()
	}
	return nil
}

// Raw exposes the underlying *pebble.DB for tests and migrations.
// Production code should go through the wrapper.
func (d *DB) Raw() *pebbledb.DB { return d.pebble }

// IsLeader reports whether this node is the current raft leader. When
// raft is not yet wired (M1 scaffold), returns true so callers behave
// as if single-node.
func (d *DB) IsLeader() bool {
	if d.raft == nil {
		return true
	}
	return d.raft.State() == hraft.Leader
}

// LeaderObservation returns a channel that receives true when this node
// becomes the leader, false when it loses leadership. Used by the reaper
// to gate leader-only sweeps.
func (d *DB) LeaderObservation() <-chan bool { return d.leaderCh }

// WaitFollower blocks until this node is NOT the leader, or ctx is done.
// Useful in tests that want to force a failover handoff.
func (d *DB) WaitFollower(ctx context.Context) error {
	for d.IsLeader() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.leaderCh:
			// loop and re-check IsLeader
		}
	}
	return nil
}

// NextSeq returns a monotonically increasing seq for ordering pending
// entries within a priority bucket. Only the leader allocates seq —
// followers receive the value embedded in the replicated batch.
func (d *DB) NextSeq() uint64 { return d.seq.Add(1) }

// recoverSeq scans pending keys to seed the seq counter. Same logic as
// pebble.DB.recoverSeq — sharing code via direct port until T7 unifies.
func (d *DB) recoverSeq() error {
	// TODO(M1.T6): port pebble.DB.recoverSeq logic here. For now no-op
	// — the value will be set on first leader promotion.
	return nil
}

// Health is a cheap liveness probe.
func (d *DB) Health(ctx context.Context) error {
	if d == nil || d.pebble == nil {
		return fmt.Errorf("db not open")
	}
	_, closer, err := d.pebble.Get([]byte("codeq/__health__"))
	if err == pebbledb.ErrNotFound {
		_ = ctx
		return nil
	}
	if err != nil {
		return err
	}
	closer.Close()
	return nil
}

// ErrNotFound mirrors pebble.ErrNotFound so callers don't need to import
// pebbledb.
var ErrNotFound = pebbledb.ErrNotFound

// --- read pass-through (always local) ---

func (d *DB) Get(key []byte) ([]byte, error) {
	v, closer, err := d.pebble.Get(key)
	if err == pebbledb.ErrNotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(v))
	copy(out, v)
	closer.Close()
	return out, nil
}

func (d *DB) Has(key []byte) (bool, error) {
	_, closer, err := d.pebble.Get(key)
	if err == pebbledb.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	closer.Close()
	return true, nil
}

func (d *DB) Iter(lower, upper []byte) (*pebbledb.Iterator, error) {
	return d.pebble.NewIter(&pebbledb.IterOptions{LowerBound: lower, UpperBound: upper})
}

// --- write path (T6 will route through raft.Apply) ---

// Set writes a single key. M1 scaffold: writes go directly to local
// Pebble. T6 routes through raft.Apply.
func (d *DB) Set(key, value []byte) error {
	return d.pebble.Set(key, value, pebbledb.NoSync)
}

// Delete removes a key. M1 scaffold: direct local write. T6 routes
// through raft.Apply.
func (d *DB) Delete(key []byte) error {
	return d.pebble.Delete(key, pebbledb.NoSync)
}

// Batch returns a fresh batch the caller can populate. Caller MUST
// CommitBatch (which routes through raft) or close+discard.
func (d *DB) Batch() *pebbledb.Batch { return d.pebble.NewBatch() }

// CommitBatch routes the batch through raft. M1 scaffold: commits to
// local Pebble directly. T6 serializes the batch, calls raft.Apply, and
// the FSM commits.
func (d *DB) CommitBatch(b *pebbledb.Batch) error {
	return b.Commit(pebbledb.NoSync)
}
