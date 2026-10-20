package pebble

import (
	"context"
	"errors"

	pebbledb "github.com/cockroachdb/pebble"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func (d *DB) writeOptions() *pebbledb.WriteOptions {
	if d.fsyncOnCommit {
		return pebbledb.Sync
	}
	return pebbledb.NoSync
}

// confirmAbsoluteReplay flushes an opaque WAL fence (or commits it through
// Raft) without rewriting task state. A previous failed Sync may have left a
// task in memory; presence alone is not a durable receipt.
func (r *TaskRepository) confirmAbsoluteReplay(ctx context.Context, task *domain.Task) (*domain.Task, error) {
	if !r.db.DurableWrites() {
		return nil, errors.New("absoluteExpiry requires fsyncOnCommit=true on every storage member")
	}
	if err := r.ensureLeaderDispatch(ctx); err != nil {
		return nil, err
	}
	b := r.db.Batch()
	defer b.Close()
	if err := b.LogData([]byte("codeq:absolute-expiry-replay"), nil); err != nil {
		return nil, err
	}
	if err := r.db.CommitBatch(b); err != nil {
		return nil, err
	}
	current, err := r.Get(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if current.Status == domain.StatusPending && current.LastKnownLocation == domain.LocationPending {
		priority := normalizePriority(current.Priority)
		lo, hi := PrefixPendingPrio(current.Command, current.TenantID, priority)
		key, err := r.findIndexEntry(lo, hi, current.ID)
		if err != nil {
			return nil, err
		}
		if key != nil {
			r.publishPending(current.Command, current.TenantID, priority, beUint64(key[len(lo):len(lo)+8]), current.ID)
		}
	}
	return current, nil
}

// DurableWrites verifies the local persistence setting and replication owner.
// Every Raft member must run the same durable setting before opt-in activation.
func (d *DB) DurableWrites() bool {
	if !d.fsyncOnCommit {
		return false
	}
	if d.repl == nil {
		return true
	}
	owner, ok := d.repl.(interface{ DurableWrites() bool })
	return ok && owner.DurableWrites()
}
