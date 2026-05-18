package pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
)

// Replicator is satisfied by anything that knows how to ship a
// pebble.Batch.Repr through a replication protocol (raft, etc.) and
// wait for it to apply locally. When AttachReplicator is called on DB,
// every write — Set, Delete, CommitBatch — flows through Replicate
// instead of committing the local pebble.Batch directly. The same
// pebble store backs both ends (the FSM on the receive side applies to
// d.db), so local reads via Get/Iter still see the replicated state.
type Replicator interface {
	IsLeader() bool
	Replicate(repr []byte) error
}

// ErrNotLeader is returned by write APIs when this DB is attached to a
// replicator and the local node isn't the leader. The repository code
// surfaces it to the service layer; the cluster router (not the storage
// layer) decides whether to forward to a leader.
var ErrNotLeader = errors.New("pebble: not leader")

// DB wraps a *pebble.DB with the helpers the repository implementations
// need: a process-wide monotonic sequence counter (used to order pending
// queue entries within a priority bucket) and a few convenience methods
// for atomic batch writes.
//
// DB is safe for concurrent use; pebble itself is goroutine-safe and the
// seq counter uses atomics.
//
// Group commit: CommitBatch submits to a single coalescer goroutine that
// merges concurrently-arriving batches into one larger Pebble batch
// before calling Commit. Phase 0 profiling pinned Pebble's internal
// commitPipeline mutex at 96% of mutex profile and 44% of block profile
// at 26k req/s — every Commit() acquires that global lock. Merging N
// batches into one Commit collapses N lock acquisitions into one, which
// is the primary throughput lever on the Pebble write path.
//
// Replication: when AttachReplicator is called, the coalescer is
// bypassed and every write goes through repl.Replicate(batch.Repr()).
// The replicator (raft) does its own batching at the log-entry level,
// so coalescing on top would just add latency.
type DB struct {
	db       *pebbledb.DB
	seq      atomic.Uint64
	commitCh chan *commitReq
	stopCh   chan struct{}
	stopped  chan struct{}

	// repl is the optional replication delegate. nil = direct pebble
	// mode (the standalone path the bench harness uses). Set by
	// AttachReplicator after construction.
	repl Replicator

	// Leases is the Phase 6 / M2 in-memory lease table, shared between
	// TaskRepository (writer) and ResultRepository (clearer on submit).
	// Lives on DB so both repos see the same instance without an extra
	// wiring layer. Recovery from KeyInprog runs in
	// TaskRepository.recoverLeases at NewTaskRepository time.
	Leases *leaseTable
}

// AttachReplicator hooks a replication delegate (typically *raft.DB)
// onto this DB. After this call returns, every Set / Delete /
// CommitBatch flows through r.Replicate instead of the local coalescer.
// Reads are unaffected (always local). Must be called before any
// repositories start writing — concurrent attach + write is not safe.
func (d *DB) AttachReplicator(r Replicator) { d.repl = r }

// commitReq carries a single submitter's batch through the coalescer.
// done is buffered so the coalescer never blocks on fan-out.
type commitReq struct {
	batch *pebbledb.Batch
	done  chan error
}

const (
	// maxMergeBatch caps the number of batches merged into a single
	// Pebble commit. Higher values amortize the commitPipeline mutex
	// over more ops but also increase tail latency for late joiners and
	// the merged batch's memory footprint. 64 is a guess we can tune.
	maxMergeBatch = 64
	// commitChanBuf bounds the queue depth between producers and the
	// coalescer goroutine. Sized to absorb several commit cycles' worth
	// of in-flight batches at peak load (k6 saturation around 26-30k
	// req/s × 3 commits/task ≈ ~90k commits/s; the coalescer drains
	// fast enough that 1024 hasn't blocked in practice).
	commitChanBuf = 1024
)

// Options is a thin facade over pebble.Options so callers don't need to
// import pebble directly for basic open/close.
type Options struct {
	Path string
	// FsyncOnCommit forces fsync on every batch commit. Defaults to false
	// (NoSync) for max throughput; the bench can flip it on to compare.
	FsyncOnCommit bool
}

// Open creates or opens the Pebble database at opts.Path. Pebble acquires
// an exclusive lock on the directory — only one process may hold it.
func Open(opts Options) (*DB, error) {
	pOpts := &pebbledb.Options{
		// Bloom filters speed up negative point lookups (HGET-style misses
		// on the ghost path). Bits-per-key 10 is the standard trade-off
		// (~1% FP rate, ~10 bits per key memory).
		Cache: pebbledb.NewCache(256 << 20), // 256 MiB block cache
	}
	// Single L0 level option to favor write throughput in our queue workload.
	for i := range pOpts.Levels {
		pOpts.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
		pOpts.Levels[i].FilterType = pebbledb.TableFilter
	}

	d, err := pebbledb.Open(opts.Path, pOpts)
	if err != nil {
		return nil, fmt.Errorf("pebble open %s: %w", opts.Path, err)
	}

	wrapper := &DB{
		db:       d,
		commitCh: make(chan *commitReq, commitChanBuf),
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
		Leases:   newLeaseTable(),
	}
	if err := wrapper.recoverSeq(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("recover seq: %w", err)
	}
	go wrapper.commitLoop()
	return wrapper, nil
}

func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	// Stop the coalescer first so any in-flight CommitBatch caller gets
	// its response (or db-closed error) before pebble.Close yanks the DB
	// out from under it.
	close(d.stopCh)
	<-d.stopped
	return d.db.Close()
}

// Raw exposes the underlying pebble.DB for advanced callers (tests,
// migrations). Production repository code should go through the helpers.
func (d *DB) Raw() *pebbledb.DB { return d.db }

// NextSeq returns a monotonically increasing uint64 used to order entries
// within a pending priority bucket. Sequence numbers are unique within a
// process lifetime; on restart we recover the high-water mark from the
// existing pending keys so new enqueues sort after old ones.
func (d *DB) NextSeq() uint64 { return d.seq.Add(1) }

// recoverSeq scans all pending keys and sets seq to max+1. Linear in the
// pending-queue size at startup; for very large queues we could persist a
// checkpoint, but the workloads we care about fit comfortably.
func (d *DB) recoverSeq() error {
	lower := []byte(pQueue)
	upper := prefixUpper(lower)
	iter, err := d.db.NewIter(&pebbledb.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return err
	}
	defer iter.Close()

	var maxSeq uint64
	pendingMarker := []byte(segPending)
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		idx := bytes.Index(k, pendingMarker)
		if idx < 0 {
			continue
		}
		// key = ".../pending/<prio_be1>/<seq_be8>/<id>"
		// seq starts at idx + len(segPending) + 1 (prio byte) + 1 ('/')
		seqStart := idx + len(pendingMarker) + 1 + 1
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

// ---------- basic KV ops (thin wrappers) ----------

// Get returns the value at key. Returns (nil, ErrNotFound) on miss.
func (d *DB) Get(key []byte) ([]byte, error) {
	v, closer, err := d.db.Get(key)
	if err == pebbledb.ErrNotFound {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Copy out before closer.Close() — pebble may reuse the buffer.
	out := make([]byte, len(v))
	copy(out, v)
	closer.Close()
	return out, nil
}

// Has reports membership without copying the value. Pebble has no direct
// Exists, so we do a Get and discard.
func (d *DB) Has(key []byte) (bool, error) {
	_, closer, err := d.db.Get(key)
	if err == pebbledb.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	closer.Close()
	return true, nil
}

// Set writes a single key/value. With a replicator attached the write
// flows through Replicate (raft) as a 1-op batch; otherwise it commits
// directly with no-sync.
func (d *DB) Set(key, value []byte) error {
	if d.repl != nil {
		if !d.repl.IsLeader() {
			return ErrNotLeader
		}
		b := d.db.NewBatch()
		defer b.Close()
		if err := b.Set(key, value, nil); err != nil {
			return err
		}
		return d.repl.Replicate(b.Repr())
	}
	return d.db.Set(key, value, pebbledb.NoSync)
}

// Delete removes a key. Same semantics as Set re: replication.
func (d *DB) Delete(key []byte) error {
	if d.repl != nil {
		if !d.repl.IsLeader() {
			return ErrNotLeader
		}
		b := d.db.NewBatch()
		defer b.Close()
		if err := b.Delete(key, nil); err != nil {
			return err
		}
		return d.repl.Replicate(b.Repr())
	}
	return d.db.Delete(key, pebbledb.NoSync)
}

// Batch returns a fresh batch the caller can populate and commit. Callers
// MUST call b.Close() (defer) and either b.Commit(...) or discard.
func (d *DB) Batch() *pebbledb.Batch {
	return d.db.NewBatch()
}

// CommitBatch hands the batch to the group-commit coalescer and blocks
// until the merged commit finishes. From the caller's perspective the
// contract is identical to before (synchronous, atomic, returns the
// commit error if any). Under the hood up to maxMergeBatch concurrent
// callers share a single Pebble Commit, slashing commitPipeline mutex
// contention.
//
// Caller still owns the original batch and MUST Close it after this
// returns (typical defer b.Close()). Apply() copies the ops out, so
// closing the caller's batch does not affect the merged commit.
func (d *DB) CommitBatch(b *pebbledb.Batch) error {
	if d.repl != nil {
		if !d.repl.IsLeader() {
			return ErrNotLeader
		}
		// Replicator owns serialization; the local pebble write happens
		// on every node via the FSM. The caller still owns b and must
		// Close it (the standard defer-Close pattern).
		return d.repl.Replicate(b.Repr())
	}
	req := &commitReq{batch: b, done: make(chan error, 1)}
	select {
	case d.commitCh <- req:
	case <-d.stopCh:
		return fmt.Errorf("db closed")
	}
	return <-req.done
}

// commitLoop is the single goroutine that owns the Pebble write side.
// It pops the first queued request (blocking), opportunistically drains
// additional requests already queued, merges them all into one batch,
// and issues a single Commit. Errors fan out to every joined submitter.
//
// Tail-latency note: a submitter that arrives just after the coalescer
// committed pays one merge cycle of wait. With maxMergeBatch=64 and a
// per-commit cost on the order of microseconds, this is the same order
// of magnitude as the lock wait we were already paying pre-coalescer.
// We're betting throughput here, not latency.
func (d *DB) commitLoop() {
	defer close(d.stopped)
	for {
		var first *commitReq
		select {
		case <-d.stopCh:
			// Drain any queued requests with a closed-DB error so
			// callers don't block forever.
			for {
				select {
				case req := <-d.commitCh:
					req.done <- fmt.Errorf("db closed")
				default:
					return
				}
			}
		case first = <-d.commitCh:
		}

		merged := d.db.NewBatch()
		if err := merged.Apply(first.batch, nil); err != nil {
			first.done <- err
			_ = merged.Close()
			continue
		}
		reqs := []*commitReq{first}

	drain:
		for len(reqs) < maxMergeBatch {
			select {
			case more := <-d.commitCh:
				if err := merged.Apply(more.batch, nil); err != nil {
					// Stop merging on Apply failure; this submitter
					// gets the error directly, and we commit whatever
					// merged so far.
					more.done <- err
					break drain
				}
				reqs = append(reqs, more)
			default:
				break drain
			}
		}

		err := merged.Commit(pebbledb.NoSync)
		_ = merged.Close()
		for _, r := range reqs {
			r.done <- err
		}
	}
}

// Iter returns a new iterator scoped to [lower, upper). Caller MUST Close.
func (d *DB) Iter(lower, upper []byte) (*pebbledb.Iterator, error) {
	return d.db.NewIter(&pebbledb.IterOptions{LowerBound: lower, UpperBound: upper})
}

// Health is a cheap liveness probe; reads a non-existent key to exercise
// the engine without touching real data.
func (d *DB) Health(ctx context.Context) error {
	if d == nil || d.db == nil {
		return fmt.Errorf("db not open")
	}
	_, _, err := d.db.Get([]byte("codeq/__health__"))
	if err != nil && err != pebbledb.ErrNotFound {
		return err
	}
	_ = ctx
	return nil
}

// ErrNotFound is the sentinel returned by Get/Has when a key is missing.
// Mirrors the pkg/persistence sentinel so callers don't need to import pebbledb.
var ErrNotFound = pebbledb.ErrNotFound
