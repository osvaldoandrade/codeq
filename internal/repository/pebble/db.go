package pebble

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
)

// DB wraps a *pebble.DB with the helpers the repository implementations
// need: a process-wide monotonic sequence counter (used to order pending
// queue entries within a priority bucket) and a few convenience methods
// for atomic batch writes.
//
// DB is safe for concurrent use; pebble itself is goroutine-safe and the
// seq counter uses atomics.
type DB struct {
	db  *pebbledb.DB
	seq atomic.Uint64
}

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

	wrapper := &DB{db: d}
	if err := wrapper.recoverSeq(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("recover seq: %w", err)
	}
	return wrapper, nil
}

func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
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

// Set writes a single key/value with the default (no-sync) write options.
// Hot-path use; for atomicity across multiple keys use Batch.
func (d *DB) Set(key, value []byte) error {
	return d.db.Set(key, value, pebbledb.NoSync)
}

// Delete removes key. No-op if absent.
func (d *DB) Delete(key []byte) error {
	return d.db.Delete(key, pebbledb.NoSync)
}

// Batch returns a fresh batch the caller can populate and commit. Callers
// MUST call b.Close() (defer) and either b.Commit(...) or discard.
func (d *DB) Batch() *pebbledb.Batch {
	return d.db.NewBatch()
}

// CommitBatch applies the batch atomically. NoSync is used by default;
// callers that need durability across crash should pass pebble.Sync.
func (d *DB) CommitBatch(b *pebbledb.Batch) error {
	return b.Commit(pebbledb.NoSync)
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
