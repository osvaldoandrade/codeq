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
	PeerAddrs       map[string]string // id → raft bind addr
	// PeerHTTPAddrs maps peer ID → HTTP base URL ("http://host:port").
	// Optional. When set, LeaderHTTPAddr() returns the leader's HTTP
	// URL so the status endpoint + smart clients can route writes
	// directly to the leader.
	PeerHTTPAddrs   map[string]string
	HeartbeatMS     int               // raft heartbeat (default 1000)
	ElectionMS      int               // raft election (default 1000)
	LeaderLeaseMS   int               // raft leader lease (default 500)
	CommitMS        int               // raft commit timeout (default 50)
	SnapshotEntries uint64            // log entries before snapshot (default 8192)
	ApplyTimeout    time.Duration     // per-write raft.Apply timeout (default 10s)
	// StreamLayer is an optional override for the underlying transport
	// layer. When non-nil (mux mode), Open uses
	// hraft.NewNetworkTransport on top of it instead of opening its own
	// TCP listener via NewTCPTransport. Used by the per-node
	// MuxAcceptor so every shard shares one listener.
	StreamLayer     hraft.StreamLayer
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
	pebble    *pebbledb.DB
	ownPebble bool // true when Open() opened pebble; false when caller did
	raft      *hraft.Raft
	fsm       *fsm
	trans     hraft.Transport
	cfg       Config

	seq atomic.Uint64

	leaderCh chan bool
	stopCh   chan struct{}

	// Apply coalescer: Replicate() submits to applyCh; a single loop
	// goroutine pops requests, merges concurrent ones into one Pebble
	// batch, and submits a single raft.Apply with the merged Repr.
	// This collapses N small log entries and N FSM Apply commits into
	// one big entry and one commit — the same trade pkg/repository
	// pebble.commitLoop makes for the non-raft path. See applyLoop.
	applyCh      chan *applyReq
	applyStopped chan struct{}
}

// applyReq carries a single submitter's serialized pebble batch through
// the coalescer. done is buffered so the loop never blocks fanning out.
type applyReq struct {
	repr []byte
	done chan error
}

const (
	// raftMergeBatch caps how many concurrent Replicate calls coalesce
	// into a single raft.Apply. 128 keeps tail-latency bounded while
	// amortising the per-Apply overhead (log append, AE round-trip,
	// FSM Apply, Pebble commit) across many submitters. Empirical sweet
	// spot — at 256 the merged batch exceeds raft's preferred entry
	// size and throughput drops back to ~10k cycles/s.
	raftMergeBatch = 128

	// raftApplyChanBuf bounds the queue between Replicate callers and
	// the apply loop. Sized for a few merge cycles of headroom under
	// burst.
	raftApplyChanBuf = 1024
)

// Open creates or opens a raft-pebble DB. The Pebble store lives under
// cfg.Path/state/; the raft FileSnapshotStore lives under
// cfg.Path/snapshots/. Log + stable state share the Pebble store with
// the FSM under separate prefixes (raft/log/, raft/stable/).
//
// Use OpenWithPebble when the calling layer already owns a *pebble.DB
// it wants to attach raft replication to (the codeq integration path —
// see pkg/app/application_pebble.go).
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("raft: Path is required")
	}
	pOpts := &pebbledb.Options{Cache: pebbledb.NewCache(256 << 20)}
	for i := range pOpts.Levels {
		pOpts.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
		pOpts.Levels[i].FilterType = pebbledb.TableFilter
	}
	statePath := cfg.Path + "/state"
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir state: %w", err)
	}
	pdb, err := pebbledb.Open(statePath, pOpts)
	if err != nil {
		return nil, fmt.Errorf("pebble open %s: %w", statePath, err)
	}
	d, err := openInternal(ctx, cfg, pdb, true)
	if err != nil {
		_ = pdb.Close()
		return nil, err
	}
	return d, nil
}

// OpenWithPebble wires raft replication on top of a *pebble.DB the
// caller already opened. The caller keeps ownership of pdb (raft.DB
// won't close it). The same pebble instance backs the FSM, LogStore,
// StableStore, and the caller's reads/writes — under distinct prefixes
// so they don't collide (codeq/* user state, raft/log/*, raft/stable/*).
func OpenWithPebble(ctx context.Context, cfg Config, pdb *pebbledb.DB) (*DB, error) {
	if pdb == nil {
		return nil, fmt.Errorf("raft: pdb is required")
	}
	return openInternal(ctx, cfg, pdb, false)
}

func openInternal(ctx context.Context, cfg Config, pdb *pebbledb.DB, ownPebble bool) (*DB, error) {
	if cfg.SelfID == "" {
		return nil, fmt.Errorf("raft: SelfID is required")
	}
	if cfg.BindAddr == "" {
		return nil, fmt.Errorf("raft: BindAddr is required")
	}
	if cfg.Path == "" {
		return nil, fmt.Errorf("raft: Path is required (for snapshot dir)")
	}

	d := &DB{
		pebble:       pdb,
		ownPebble:    ownPebble,
		cfg:          cfg,
		leaderCh:     make(chan bool, 8),
		stopCh:       make(chan struct{}),
		applyCh:      make(chan *applyReq, raftApplyChanBuf),
		applyStopped: make(chan struct{}),
	}

	logs := newLogStore(pdb)
	stable := newStableStore(pdb)
	snaps, err := newSnapshotStore(cfg.Path + "/snapshots")
	if err != nil {
		return nil, fmt.Errorf("snapshot store: %w", err)
	}
	d.fsm = newFSM(pdb)

	var trans hraft.Transport
	if cfg.StreamLayer != nil {
		// Mux mode: the caller pre-built a StreamLayer that demuxes
		// connections from a shared listener. NewNetworkTransport
		// drives raft's wire protocol on top.
		trans = hraft.NewNetworkTransport(cfg.StreamLayer, 3, 10*time.Second, os.Stderr)
	} else {
		t, err := hraft.NewTCPTransport(cfg.BindAddr, nil, 3, 10*time.Second, os.Stderr)
		if err != nil {
			return nil, fmt.Errorf("tcp transport: %w", err)
		}
		trans = t
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
			return nil, fmt.Errorf("HasExistingState: %w", err)
		}
		if !hasState {
			peers := cfg.PeerAddrs
			if len(peers) == 0 {
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
				return nil, fmt.Errorf("BootstrapCluster: %w", err)
			}
		}
	}

	r, err := hraft.NewRaft(rcfg, d.fsm, logs, stable, snaps, trans)
	if err != nil {
		return nil, fmt.Errorf("NewRaft: %w", err)
	}
	d.raft = r

	go d.forwardLeaderChanges()
	go d.applyLoop()
	if err := d.recoverSeq(); err != nil {
		_ = d.shutdownRaftOnly()
		return nil, fmt.Errorf("recover seq: %w", err)
	}

	_ = ctx
	return d, nil
}

// shutdownRaftOnly stops raft+transport without closing pebble. Used
// when openInternal needs to back out an error before the caller takes
// ownership of the DB.
func (d *DB) shutdownRaftOnly() error {
	select {
	case <-d.stopCh:
	default:
		close(d.stopCh)
	}
	if d.raft != nil {
		_ = d.raft.Shutdown().Error()
	}
	if closer, ok := d.trans.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	return nil
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

// Close stops raft, closes the transport, and (if Open opened the
// pebble store) closes pebble too. Callers that used OpenWithPebble
// keep ownership of pebble and must close it themselves.
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
	if closer, ok := d.trans.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	if d.ownPebble && d.pebble != nil {
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

// LeaderInfo returns the current leader's id and bind address according
// to local state. Both are empty if no leader is known (election in
// progress). This is the local view — under network partition it can
// disagree with other nodes for a brief window.
func (d *DB) LeaderInfo() (id, addr string) {
	if d.raft == nil {
		return "", ""
	}
	rawAddr, rawID := d.raft.LeaderWithID()
	return string(rawID), string(rawAddr)
}

// LeaderHTTPAddr returns the leader's HTTP base URL when cfg.PeerHTTPAddrs
// has an entry for the leader, or "" otherwise. Used by the status
// endpoint and (future) smart clients to route writes directly at the
// leader's HTTP listener instead of relying on retry-on-not-leader.
func (d *DB) LeaderHTTPAddr() string {
	id, _ := d.LeaderInfo()
	if id == "" || len(d.cfg.PeerHTTPAddrs) == 0 {
		return ""
	}
	return d.cfg.PeerHTTPAddrs[id]
}

// SelfID returns the configured raft ServerID for this node.
func (d *DB) SelfID() string { return d.cfg.SelfID }

// BindAddr returns the raft bind address configured for this node.
func (d *DB) BindAddr() string { return d.cfg.BindAddr }

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

// Replicate ships a serialized pebble.Batch through raft and waits for
// it to commit + apply locally. This is the public entry point for
// callers that own their own pebble (via OpenWithPebble) and only want
// the replication primitive — the codeq integration in pkg/app uses
// this from internal/repository/pebble.DB to keep the existing
// repository surface unchanged while writes flow through raft.
//
// Returns ErrNotLeader if this node isn't the leader, or wraps the
// raft Apply error otherwise.
//
// Concurrent calls coalesce through the apply loop: multiple in-flight
// Replicates queue on applyCh and the loop merges up to raftMergeBatch
// of them into a single raft.Apply. Each caller still gets its own
// done channel and an independent error response.
func (d *DB) Replicate(repr []byte) error {
	if !d.IsLeader() {
		return ErrNotLeader
	}
	if len(repr) == 0 {
		return nil
	}
	req := &applyReq{
		repr: repr,
		done: make(chan error, 1),
	}
	select {
	case d.applyCh <- req:
	case <-d.stopCh:
		return fmt.Errorf("raft: db closing")
	}
	return <-req.done
}

// applyLoop is the single owner of d.raft.Apply. It pops the first
// queued request (blocking), opportunistically drains additional
// requests already in flight, merges them all into one Pebble batch,
// and submits a single raft entry. The merged Repr replays inside the
// FSM exactly as if each submitter had called Apply on its own — pebble
// batches are append-only collections of point ops, so merging them
// (via Batch.Apply) preserves all original writes. Errors fan out to
// every joined submitter.
//
// Ordering: producer batches are independent (different task UUIDs →
// different keys), so the merge order within a single apply call is a
// non-issue. Across apply calls raft's log ordering guarantees the
// usual sequential semantics.
//
// Tail-latency note: a submitter that arrives just after the loop
// kicked off a merge pays one cycle of wait. With raftMergeBatch=32
// and a per-Apply cost dominated by AE round-trip + FSM commit, this
// is small relative to the per-merge savings.
func (d *DB) applyLoop() {
	defer close(d.applyStopped)
	for {
		var first *applyReq
		select {
		case <-d.stopCh:
			// Drain any queued submitters with a closed-DB error so
			// callers don't block forever.
			for {
				select {
				case req := <-d.applyCh:
					req.done <- fmt.Errorf("raft: db closing")
				default:
					return
				}
			}
		case first = <-d.applyCh:
		}

		merged := d.pebble.NewBatch()
		if err := merged.SetRepr(append([]byte(nil), first.repr...)); err != nil {
			first.done <- fmt.Errorf("raft apply: setrepr first: %w", err)
			_ = merged.Close()
			continue
		}
		reqs := []*applyReq{first}

	drain:
		for len(reqs) < raftMergeBatch {
			select {
			case more := <-d.applyCh:
				tmp := d.pebble.NewBatch()
				if err := tmp.SetRepr(append([]byte(nil), more.repr...)); err != nil {
					more.done <- fmt.Errorf("raft apply: setrepr merge: %w", err)
					_ = tmp.Close()
					break drain
				}
				if err := merged.Apply(tmp, nil); err != nil {
					more.done <- fmt.Errorf("raft apply: merge: %w", err)
					_ = tmp.Close()
					break drain
				}
				_ = tmp.Close()
				reqs = append(reqs, more)
			default:
				break drain
			}
		}

		// Leadership can change between Replicate's check and here.
		// Bail out cleanly if so — submitters retry on a different
		// node via the existing not-leader path.
		if !d.IsLeader() {
			_ = merged.Close()
			for _, r := range reqs {
				r.done <- ErrNotLeader
			}
			continue
		}

		cp := append([]byte(nil), merged.Repr()...)
		_ = merged.Close()
		f := d.raft.Apply(cp, d.cfg.applyTimeout())
		err := f.Error()
		if err != nil {
			err = fmt.Errorf("raft apply: %w", err)
		} else if resp := f.Response(); resp != nil {
			if applyErr, ok := resp.(error); ok && applyErr != nil {
				err = applyErr
			}
		}
		for _, r := range reqs {
			r.done <- err
		}
	}
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

// Compile-time check that *DB satisfies the repository-layer
// Replicator contract. Kept here (not in pebble/) to avoid pulling
// pebblerepo into the raft package's import graph.
type pebbleReplicator interface {
	IsLeader() bool
	Replicate(repr []byte) error
}

var _ pebbleReplicator = (*DB)(nil)

// netAddrFromString resolves "host:port" to net.Addr. Used by LocalAddr
// for tests that need to learn the ephemeral port the transport chose.
func netAddrFromString(s string) net.Addr {
	addr, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		return nil
	}
	return addr
}
