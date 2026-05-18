// Package raft provides a Pebble-backed, Raft-replicated KV with the
// same surface as internal/repository/pebble.DB. Writes flow through
// raft.Apply and land in the local Pebble store via the FSM; reads are
// local (and may be stale on followers).
package raft

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
	hraft "github.com/hashicorp/raft"
)

// Config configures the raft-pebble DB. PeerAddrs lists every peer in
// the cluster (including self) by stable ID and bind address. SelfID
// must appear in PeerAddrs. Bootstrap=true makes this node attempt to
// bootstrap a fresh cluster; only the first node started in a new
// deployment should do this.
type Config struct {
	Path            string
	FsyncOnCommit   bool
	SelfID          string
	BindAddr        string
	Bootstrap       bool
	PeerAddrs       map[string]string // id → bind addr
	HeartbeatMS     int               // raft heartbeat (default 1000)
	ElectionMS      int               // raft election (default 1000)
	LeaderLeaseMS   int               // raft leader lease (default 500)
	CommitMS        int               // raft commit timeout (default 50)
	SnapshotEntries uint64            // log entries before snapshot (default 8192)
	ApplyTimeout    time.Duration     // per-write raft.Apply timeout (default 10s)
}

func (c Config) heartbeat() time.Duration {
	if c.HeartbeatMS > 0 {
		return time.Duration(c.HeartbeatMS) * time.Millisecond
	}
	return 1000 * time.Millisecond
}

func (c Config) election() time.Duration {
	if c.ElectionMS > 0 {
		return time.Duration(c.ElectionMS) * time.Millisecond
	}
	return 1000 * time.Millisecond
}

func (c Config) leaderLease() time.Duration {
	if c.LeaderLeaseMS > 0 {
		return time.Duration(c.LeaderLeaseMS) * time.Millisecond
	}
	return 500 * time.Millisecond
}

func (c Config) commitTimeout() time.Duration {
	if c.CommitMS > 0 {
		return time.Duration(c.CommitMS) * time.Millisecond
	}
	return 50 * time.Millisecond
}

func (c Config) snapshotInterval() time.Duration {
	return 120 * time.Second
}

func (c Config) snapshotThreshold() uint64 {
	if c.SnapshotEntries > 0 {
		return c.SnapshotEntries
	}
	return 8192
}

func (c Config) applyTimeout() time.Duration {
	if c.ApplyTimeout > 0 {
		return c.ApplyTimeout
	}
	return 10 * time.Second
}

// ErrNotLeader is returned by write APIs when this node is not the
// current raft leader. The repository layer is expected to surface this
// to clients (who may retry against a different node) rather than
// transparently forwarding — that's a cluster-router concern.
var ErrNotLeader = errors.New("raft: not leader")

// ErrNotFound mirrors pebble.ErrNotFound so callers don't need to import
// pebbledb.
var ErrNotFound = pebbledb.ErrNotFound

// DB is a Pebble store with raft replication. Reads are local; writes
// go through the FSM. The API mirrors internal/repository/pebble.DB so
// the rest of codeq can swap one for the other.
type DB struct {
	pebble *pebbledb.DB
	raft   *hraft.Raft
	fsm    *fsm
	trans  hraft.Transport
	cfg    Config

	seq atomic.Uint64

	leaderCh chan bool
	stopCh   chan struct{}

	Leases *LeaseTable
}

// Open creates or opens a raft-pebble DB. The Pebble store lives under
// cfg.Path/state/; the raft FileSnapshotStore lives under
// cfg.Path/snapshots/. Log + stable state share the Pebble store with
// the FSM under separate prefixes (raft/log/, raft/stable/).
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

	pOpts := &pebbledb.Options{Cache: pebbledb.NewCache(256 << 20)}
	for i := range pOpts.Levels {
		pOpts.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
		pOpts.Levels[i].FilterType = pebbledb.TableFilter
	}
	statePath := cfg.Path + "/state"
	if err := os.MkdirAll(statePath, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state: %w", err)
	}
	pdb, err := pebbledb.Open(statePath, pOpts)
	if err != nil {
		return nil, fmt.Errorf("pebble open %s: %w", statePath, err)
	}

	d := &DB{
		pebble:   pdb,
		cfg:      cfg,
		leaderCh: make(chan bool, 8),
		stopCh:   make(chan struct{}),
		Leases:   newLeaseTable(),
	}

	logs := newLogStore(pdb)
	stable := newStableStore(pdb)
	snaps, err := newSnapshotStore(cfg.Path + "/snapshots")
	if err != nil {
		_ = pdb.Close()
		return nil, fmt.Errorf("snapshot store: %w", err)
	}
	d.fsm = newFSM(pdb)

	trans, err := hraft.NewTCPTransport(cfg.BindAddr, nil, 3, 10*time.Second, os.Stderr)
	if err != nil {
		_ = pdb.Close()
		return nil, fmt.Errorf("tcp transport: %w", err)
	}
	d.trans = trans

	rcfg := hraft.DefaultConfig()
	rcfg.LocalID = hraft.ServerID(cfg.SelfID)
	rcfg.HeartbeatTimeout = cfg.heartbeat()
	rcfg.ElectionTimeout = cfg.election()
	rcfg.LeaderLeaseTimeout = cfg.leaderLease()
	rcfg.CommitTimeout = cfg.commitTimeout()
	rcfg.SnapshotInterval = cfg.snapshotInterval()
	rcfg.SnapshotThreshold = cfg.snapshotThreshold()
	rcfg.LogOutput = os.Stderr

	if cfg.Bootstrap {
		hasState, err := hraft.HasExistingState(logs, stable, snaps)
		if err != nil {
			_ = pdb.Close()
			return nil, fmt.Errorf("HasExistingState: %w", err)
		}
		if !hasState {
			peers := cfg.PeerAddrs
			if len(peers) == 0 {
				// Single-node degenerate cluster: self only.
				peers = map[string]string{cfg.SelfID: cfg.BindAddr}
			}
			var configuration hraft.Configuration
			for id, addr := range peers {
				configuration.Servers = append(configuration.Servers, hraft.Server{
					Suffrage: hraft.Voter,
					ID:       hraft.ServerID(id),
					Address:  hraft.ServerAddress(addr),
				})
			}
			if err := hraft.BootstrapCluster(rcfg, logs, stable, snaps, trans, configuration); err != nil {
				_ = pdb.Close()
				return nil, fmt.Errorf("BootstrapCluster: %w", err)
			}
		}
	}

	r, err := hraft.NewRaft(rcfg, d.fsm, logs, stable, snaps, trans)
	if err != nil {
		_ = pdb.Close()
		return nil, fmt.Errorf("NewRaft: %w", err)
	}
	d.raft = r

	go d.forwardLeaderChanges()
	if err := d.recoverSeq(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("recover seq: %w", err)
	}

	_ = ctx
	return d, nil
}

// forwardLeaderChanges relays raft's LeaderCh to our buffered chan so
// the reaper (and tests) can wait on a non-blocking signal.
func (d *DB) forwardLeaderChanges() {
	src := d.raft.LeaderCh()
	for {
		select {
		case <-d.stopCh:
			return
		case isLeader, ok := <-src:
			if !ok {
				return
			}
			select {
			case d.leaderCh <- isLeader:
			default:
				// Buffer full; drop. The reaper only needs to know the
				// current state, not the full history.
			}
		}
	}
}

func (d *DB) Close() error {
	if d == nil {
		return nil
	}
	select {
	case <-d.stopCh:
	default:
		close(d.stopCh)
	}
	if d.raft != nil {
		f := d.raft.Shutdown()
		if err := f.Error(); err != nil {
			return fmt.Errorf("raft shutdown: %w", err)
		}
	}
	// Closing the transport explicitly ensures the listening socket
	// closes promptly; raft.Shutdown stops processing but the transport
	// may keep the listener open.
	if closer, ok := d.trans.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	if d.pebble != nil {
		return d.pebble.Close()
	}
	return nil
}

// Raw exposes the underlying *pebble.DB. Test/migration use only.
func (d *DB) Raw() *pebbledb.DB { return d.pebble }

// LocalAddr returns the bound address of the raft transport (useful in
// tests that pass "127.0.0.1:0" to grab an ephemeral port).
func (d *DB) LocalAddr() net.Addr {
	if d.trans == nil {
		return nil
	}
	return netAddrFromString(string(d.trans.LocalAddr()))
}

// IsLeader reports whether this node is the current raft leader.
func (d *DB) IsLeader() bool {
	if d.raft == nil {
		return false
	}
	return d.raft.State() == hraft.Leader
}

// LeaderObservation returns a channel that receives true when this node
// becomes the leader, false when it loses leadership.
func (d *DB) LeaderObservation() <-chan bool { return d.leaderCh }

// WaitLeader blocks until this node IS the leader, or ctx is done.
// Useful in tests that bootstrap and then need to write.
func (d *DB) WaitLeader(ctx context.Context) error {
	for !d.IsLeader() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.leaderCh:
			// loop and re-check
		case <-time.After(20 * time.Millisecond):
			// poll fallback in case the buffered channel dropped our
			// transition
		}
	}
	return nil
}

// WaitFollower blocks until this node is NOT the leader, or ctx is done.
// Used in failover tests.
func (d *DB) WaitFollower(ctx context.Context) error {
	for d.IsLeader() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.leaderCh:
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

// NextSeq returns a monotonically increasing seq for ordering pending
// entries within a priority bucket. Only the leader allocates seq —
// followers receive the value embedded in the replicated batch.
func (d *DB) NextSeq() uint64 { return d.seq.Add(1) }

// recoverSeq scans pending keys to seed the seq counter. Mirrors the
// logic in internal/repository/pebble/db.go::recoverSeq — the seq
// suffix lives at a fixed offset inside the pending key.
func (d *DB) recoverSeq() error {
	const pendingMarker = "/pending/"
	const queuePrefix = "codeq/q/"
	queueEnd := []byte("codeq/q0") // codeq/q + 0x30 (one past '/')
	iter, err := d.pebble.NewIter(&pebbledb.IterOptions{
		LowerBound: []byte(queuePrefix),
		UpperBound: queueEnd,
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	pendingBytes := []byte(pendingMarker)
	var maxSeq uint64
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		idx := bytes.Index(k, pendingBytes)
		if idx < 0 {
			continue
		}
		seqStart := idx + len(pendingBytes) + 1 + 1 // prio byte + '/'
		if seqStart+8 > len(k) {
			continue
		}
		s := beUint64(k[seqStart : seqStart+8])
		if s > maxSeq {
			maxSeq = s
		}
	}
	d.seq.Store(maxSeq)
	return iter.Error()
}

func beUint64(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}

// Health is a cheap liveness probe.
func (d *DB) Health(ctx context.Context) error {
	if d == nil || d.pebble == nil {
		return fmt.Errorf("db not open")
	}
	_, closer, err := d.pebble.Get([]byte("codeq/__health__"))
	if errors.Is(err, pebbledb.ErrNotFound) {
		_ = ctx
		return nil
	}
	if err != nil {
		return err
	}
	closer.Close()
	return nil
}

// --- read pass-through (always local) ---

func (d *DB) Get(key []byte) ([]byte, error) {
	v, closer, err := d.pebble.Get(key)
	if errors.Is(err, pebbledb.ErrNotFound) {
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
	if errors.Is(err, pebbledb.ErrNotFound) {
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

// --- write path (routes through raft.Apply) ---

// Batch returns a fresh batch the caller can populate. Caller MUST call
// CommitBatch (which routes through raft) and then Close.
func (d *DB) Batch() *pebbledb.Batch { return d.pebble.NewBatch() }

// CommitBatch hands the batch's Repr to raft. Blocks until the raft
// entry is committed by the quorum and applied on this node. Returns
// ErrNotLeader if this node is not currently the leader.
//
// The caller still owns the batch and must Close it after CommitBatch
// returns — the underlying write has been applied to Pebble through
// the FSM, not through the caller's batch.
func (d *DB) CommitBatch(b *pebbledb.Batch) error {
	if !d.IsLeader() {
		return ErrNotLeader
	}
	repr := b.Repr()
	if len(repr) == 0 {
		return nil
	}
	cp := make([]byte, len(repr))
	copy(cp, repr)
	f := d.raft.Apply(cp, d.cfg.applyTimeout())
	if err := f.Error(); err != nil {
		return fmt.Errorf("raft apply: %w", err)
	}
	if resp := f.Response(); resp != nil {
		if applyErr, ok := resp.(error); ok && applyErr != nil {
			return applyErr
		}
	}
	return nil
}

// Set replicates a single-key write through raft.
func (d *DB) Set(key, value []byte) error {
	if !d.IsLeader() {
		return ErrNotLeader
	}
	b := d.pebble.NewBatch()
	defer b.Close()
	if err := b.Set(key, value, nil); err != nil {
		return err
	}
	return d.CommitBatch(b)
}

// Delete replicates a single-key delete through raft.
func (d *DB) Delete(key []byte) error {
	if !d.IsLeader() {
		return ErrNotLeader
	}
	b := d.pebble.NewBatch()
	defer b.Close()
	if err := b.Delete(key, nil); err != nil {
		return err
	}
	return d.CommitBatch(b)
}

// netAddrFromString resolves "host:port" to net.Addr. Used by LocalAddr
// for tests that need to learn the ephemeral port the transport chose.
func netAddrFromString(s string) net.Addr {
	addr, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		return nil
	}
	return addr
}
