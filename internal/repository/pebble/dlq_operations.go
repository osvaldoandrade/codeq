package pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	pebbledb "github.com/cockroachdb/pebble"

	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	// dlqRequeueChunk bounds the tasks one bulk-requeue commit moves. It
	// keeps each replicated log entry small and holds each task only for
	// one commit, so claims never wait behind a long bulk call.
	dlqRequeueChunk = 64
	// reservePoll is how often an admin operation re-checks a task (or a
	// delayed bucket) that a claim or another operation holds. Holders keep
	// it for one read and one commit.
	reservePoll = time.Millisecond
	// scoreLen is the width of the sequence or score segment that pending
	// and delayed index keys carry before "/<id>".
	scoreLen = 8
)

// readyEntry locates the pending index entry of a task and the claim hint
// that points at it.
type readyEntry struct {
	cmd      domain.Command
	tenantID string
	prio     int
	seq      uint64
}

// requeuedTask is a task staged back to the ready queue with its new
// pending sequence, published as a claim hint once the batch commits.
type requeuedTask struct {
	task *domain.Task
	seq  uint64
}

// ---------- reservation ----------

// waitUntil polls try until it succeeds or ctx ends.
func waitUntil(ctx context.Context, try func() bool) error {
	for !try() {
		timer := time.NewTimer(reservePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// tryReserve marks taskID in flight unless a claim or another admin
// operation already moves it. It is the in-flight set claims use: popHint
// skips (and drops the hint of) an in-flight task, and a claim keeps its own
// mark until its commit. So a reserved task cannot be claimed, and a task
// that is being claimed cannot be reserved.
func (r *TaskRepository) tryReserve(taskID string) bool {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	if _, busy := r.inflight[taskID]; busy {
		return false
	}
	r.inflight[taskID] = struct{}{}
	return true
}

func (r *TaskRepository) reserve(ctx context.Context, taskID string) error {
	return waitUntil(ctx, func() bool { return r.tryReserve(taskID) })
}

func (r *TaskRepository) release(taskID string) {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	delete(r.inflight, taskID)
}

// lockDelayed takes the delayed-move flag of (cmd, tenant), waiting while
// the delayed sweep holds it, so the sweep cannot move a task out of that
// bucket until the returned unlock runs.
func (r *TaskRepository) lockDelayed(ctx context.Context, cmd domain.Command, tenantID string) (func(), error) {
	flag := r.delayedMoveFlagFor(cmd, tenantID)
	if err := waitUntil(ctx, func() bool { return flag.CompareAndSwap(0, 1) }); err != nil {
		return nil, err
	}
	return func() { flag.Store(0) }, nil
}

// ---------- requeue one task ----------

// RequeueDLQTask moves a dead-lettered task back to the tail of the ready
// bucket of its priority. The task restarts as a new run: PENDING, no
// attempts, no error, no worker and no stored result. Its dead-letter entry
// is removed in the same replicated batch. The task is reserved like a claim
// for the whole read-check-write.
func (r *TaskRepository) RequeueDLQTask(ctx context.Context, taskID string) (*domain.Task, error) {
	if err := r.ensureLeaderDispatch(ctx); err != nil {
		return nil, err
	}
	if err := r.reserve(ctx, taskID); err != nil {
		return nil, err
	}
	task, seq, err := r.requeueReserved(ctx, taskID)
	r.release(taskID)
	if err != nil {
		return nil, err
	}
	r.publishRequeued(task, seq)
	return task, nil
}

func (r *TaskRepository) requeueReserved(ctx context.Context, taskID string) (*domain.Task, uint64, error) {
	task, err := r.Get(ctx, taskID)
	if err != nil {
		return nil, 0, err
	}
	dlqKey := KeyDLQ(task.Command, task.TenantID, taskID)
	inDLQ, err := r.db.Has(dlqKey)
	if err != nil {
		return nil, 0, err
	}
	if task.Status != domain.StatusFailed || !inDLQ {
		return nil, 0, domain.ErrTaskNotInDLQ
	}
	b := r.db.Batch()
	defer b.Close()
	seq, err := r.stageRequeue(b, task, dlqKey)
	if err != nil {
		return nil, 0, err
	}
	if err := r.db.CommitBatch(b); err != nil {
		return nil, 0, fmt.Errorf("commit requeue: %w", err)
	}
	return task, seq, nil
}

// stageRequeue rewrites task as a fresh PENDING run and adds to b the writes
// that move it from dlqKey to the tail of its ready bucket. It returns the
// pending sequence of the new entry.
//
// The deduplication key (ADR 0004) is not re-acquired: a requeue retries the
// task, like a nack retry, and is not a new create. The key was released when
// the task was first claimed and may now be held by a newer waiting task of
// the same key; DeduplicationKey stays on the body as a record, and
// releaseDedupe leaves the mapping alone unless it names this task.
func (r *TaskRepository) stageRequeue(b *pebbledb.Batch, task *domain.Task, dlqKey []byte) (uint64, error) {
	task.Status = domain.StatusPending
	task.LastKnownLocation = domain.LocationPending
	task.Attempts = 0
	task.Error = ""
	task.WorkerID = ""
	task.LeaseUntil = ""
	task.ResultKey = ""
	task.Progress = nil
	task.UpdatedAt = r.now()
	body, err := sonic.Marshal(task)
	if err != nil {
		return 0, fmt.Errorf("marshal task: %w", err)
	}
	seq := r.db.NextSeq()
	return seq, errors.Join(
		b.Delete(dlqKey, nil),
		b.Delete(KeyResult(task.ID), nil),
		b.Set(KeyPending(task.Command, task.TenantID, normalizePriority(task.Priority), seq, task.ID), nil, nil),
		b.Set(KeyTask(task.ID), body, nil),
	)
}

// publishRequeued makes a committed requeue claimable. The caller must have
// released the task first: a hint for an in-flight task is not sent.
func (r *TaskRepository) publishRequeued(task *domain.Task, seq uint64) {
	r.publishPending(task.Command, task.TenantID, normalizePriority(task.Priority), seq, task.ID)
	metrics.QueueDepth.WithLabelValues(string(task.Command), "ready").Inc()
}

// ---------- requeue a dead-letter queue ----------

// RequeueDLQ requeues up to limit tasks of the (cmd, tenant) dead-letter
// queue, in task ID order, committing at most dlqRequeueChunk of them per
// batch. An entry whose task is gone, is no longer FAILED or belongs to
// another queue is stale and is dropped in the same batch; an entry whose
// task another operation holds is skipped. Remaining reports whether the
// dead-letter queue still holds entries after the call. A failure keeps the
// chunks already committed.
func (r *TaskRepository) RequeueDLQ(ctx context.Context, cmd domain.Command, tenantID string, limit int) (*domain.DLQRequeue, error) {
	if err := r.ensureLeaderDispatch(ctx); err != nil {
		return nil, err
	}
	out := &domain.DLQRequeue{}
	var after []byte
	for out.Requeued < limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		moved, last, err := r.requeueDLQChunk(cmd, tenantID, after, min(limit-out.Requeued, dlqRequeueChunk))
		if err != nil {
			return nil, err
		}
		out.Requeued += moved
		if last == nil {
			break
		}
		after = last
	}
	remaining, err := r.hasDLQ(cmd, tenantID)
	if err != nil {
		return nil, err
	}
	out.Remaining = remaining
	return out, nil
}

// requeueDLQChunk handles up to want dead-letter entries after the key
// after (from the start when nil) in one batch. It returns how many tasks
// it requeued and the last entry it visited, or nil when none is left.
func (r *TaskRepository) requeueDLQChunk(cmd domain.Command, tenantID string, after []byte, want int) (int, []byte, error) {
	keys, err := r.dlqKeysAfter(cmd, tenantID, after, want)
	if err != nil || len(keys) == 0 {
		return 0, nil, err
	}
	held := r.reserveFree(keys)
	b := r.db.Batch()
	defer b.Close()
	staged, err := r.stageDLQEntries(b, cmd, tenantID, keys, held)
	if err == nil {
		err = r.db.CommitBatch(b)
	}
	for id := range held {
		r.release(id)
	}
	if err != nil {
		return 0, nil, err
	}
	for _, s := range staged {
		r.publishRequeued(s.task, s.seq)
	}
	return len(staged), keys[len(keys)-1], nil
}

// dlqKeysAfter returns up to want dead-letter keys of (cmd, tenant) that
// sort after the key after.
func (r *TaskRepository) dlqKeysAfter(cmd domain.Command, tenantID string, after []byte, want int) ([][]byte, error) {
	lower, upper := PrefixDLQ(cmd, tenantID)
	if after != nil {
		lower = append(bytes.Clone(after), 0) // the smallest key past after
	}
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	keys := make([][]byte, 0, want)
	for valid := it.First(); valid && len(keys) < want; valid = it.Next() {
		keys = append(keys, bytes.Clone(it.Key()))
	}
	return keys, it.Error()
}

// reserveFree reserves the task of every key that no claim or other
// operation holds, and returns the reserved IDs.
func (r *TaskRepository) reserveFree(keys [][]byte) map[string]struct{} {
	held := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if id := indexedID(key); r.tryReserve(id) {
			held[id] = struct{}{}
		}
	}
	return held
}

// stageDLQEntries adds to b the requeue of every held entry and the removal
// of every held entry that is stale.
func (r *TaskRepository) stageDLQEntries(b *pebbledb.Batch, cmd domain.Command, tenantID string, keys [][]byte, held map[string]struct{}) ([]requeuedTask, error) {
	staged := make([]requeuedTask, 0, len(held))
	for _, key := range keys {
		id := indexedID(key)
		if _, ok := held[id]; !ok {
			continue
		}
		task, err := r.dlqTask(id, cmd, tenantID)
		if err != nil {
			return nil, err
		}
		if task == nil {
			if err := b.Delete(key, nil); err != nil {
				return nil, err
			}
			continue
		}
		seq, err := r.stageRequeue(b, task, key)
		if err != nil {
			return nil, err
		}
		staged = append(staged, requeuedTask{task: task, seq: seq})
	}
	return staged, nil
}

// dlqTask loads the task a dead-letter entry of (cmd, tenant) points at. It
// returns nil without error when the entry is stale: the body is gone (the
// retention sweep deletes a FAILED body but not its entry), does not
// decode, is no longer FAILED, or belongs to another queue.
func (r *TaskRepository) dlqTask(id string, cmd domain.Command, tenantID string) (*domain.Task, error) {
	body, err := r.db.Get(KeyTask(id))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var task domain.Task
	decoded := sonic.Unmarshal(body, &task) == nil
	if !decoded || task.Status != domain.StatusFailed ||
		tenantSeg(task.TenantID) != tenantSeg(tenantID) || cmdSeg(task.Command) != cmdSeg(cmd) {
		return nil, nil
	}
	return &task, nil
}

// hasDLQ reports whether the (cmd, tenant) dead-letter queue holds any
// entry. It only reads, so it also answers on a Raft follower.
func (r *TaskRepository) hasDLQ(cmd domain.Command, tenantID string) (bool, error) {
	lower, upper := PrefixDLQ(cmd, tenantID)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return false, err
	}
	found := it.First()
	return found, it.Close()
}

// indexedID returns the task ID of a queue index key (its last segment).
func indexedID(key []byte) string {
	return string(key[bytes.LastIndexByte(key, '/')+1:])
}

// ---------- delete ----------

// DeleteTask removes a task that no worker holds: its body, its stored
// result and its queue index entry (ready, delayed or dead-letter), in one
// replicated batch. The task is reserved like a claim for the whole
// read-check-write, and a delayed task also holds the delayed-move flag of
// its bucket, so neither a claim nor the delayed sweep can bring it back.
// A deduplication mapping the task holds goes in the same batch. The TTL and
// idempotency entries stay; ADR 0009 explains why.
func (r *TaskRepository) DeleteTask(ctx context.Context, taskID string) error {
	if err := r.ensureLeaderDispatch(ctx); err != nil {
		return err
	}
	if err := r.reserve(ctx, taskID); err != nil {
		return err
	}
	ready, err := r.deleteReserved(ctx, taskID)
	if err != nil && ready != nil {
		// A claim drops the hint of a reserved task; put it back.
		r.finishHint(ready.cmd, ready.tenantID, ready.prio, pendingHint{seq: ready.seq, id: taskID}, true)
		return err
	}
	r.release(taskID)
	return err
}

// deleteReserved deletes a reserved task. On failure after its pending
// entry was located it returns that entry, so the caller can republish
// the claim hint.
func (r *TaskRepository) deleteReserved(ctx context.Context, taskID string) (*readyEntry, error) {
	task, err := r.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.Status == domain.StatusInProgress {
		return nil, domain.ErrTaskInProgress
	}
	b := r.db.Batch()
	defer b.Close()
	err = errors.Join(
		b.Delete(KeyTask(taskID), nil),
		b.Delete(KeyResult(taskID), nil),
		b.Delete(KeyDLQ(task.Command, task.TenantID, taskID), nil),
		b.Delete(KeyInprog(task.Command, task.TenantID, taskID), nil),
		// A waiting task that holds its deduplication key (ADR 0004)
		// releases it here. A create would already treat the key as free
		// once the body is gone, but nothing else ever removes the mapping.
		r.releaseDedupe(b, task),
	)
	switch {
	case err != nil:
		return nil, err
	case task.Status != domain.StatusPending:
		return nil, r.db.CommitBatch(b)
	case task.LastKnownLocation == domain.LocationDelayed:
		return r.deleteDelayed(ctx, b, task)
	default:
		return r.deleteReady(b, task)
	}
}

// deleteReady adds the task's pending entry to b and commits. The entry's
// sequence is not in the body, so it is found in the task's own priority
// bucket.
func (r *TaskRepository) deleteReady(b *pebbledb.Batch, task *domain.Task) (*readyEntry, error) {
	prio := normalizePriority(task.Priority)
	lower, upper := PrefixPendingPrio(task.Command, task.TenantID, prio)
	key, err := r.findIndexEntry(lower, upper, task.ID)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, r.db.CommitBatch(b)
	}
	entry := &readyEntry{cmd: task.Command, tenantID: task.TenantID, prio: prio, seq: beUint64(key[len(lower):])}
	if err := b.Delete(key, nil); err != nil {
		return entry, err
	}
	if err := r.db.CommitBatch(b); err != nil {
		return entry, err
	}
	metrics.QueueDepth.WithLabelValues(string(task.Command), "ready").Dec()
	return entry, nil
}

// deleteDelayed adds the task's delayed entry to b and commits while it
// holds the delayed-move flag of the bucket. The body is read again under
// the flag: the sweep may have made the task ready since the first read.
func (r *TaskRepository) deleteDelayed(ctx context.Context, b *pebbledb.Batch, task *domain.Task) (*readyEntry, error) {
	unlock, err := r.lockDelayed(ctx, task.Command, task.TenantID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, err := r.Get(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if current.LastKnownLocation != domain.LocationDelayed {
		return r.deleteReady(b, current)
	}
	lower, upper := PrefixDelayed(task.Command, task.TenantID)
	key, err := r.findIndexEntry(lower, upper, task.ID)
	if err != nil {
		return nil, err
	}
	if key != nil {
		if err := b.Delete(key, nil); err != nil {
			return nil, err
		}
	}
	if err := r.db.CommitBatch(b); err != nil {
		return nil, err
	}
	if key != nil {
		r.addDelayed(task.Command, task.TenantID, -1)
	}
	return nil, nil
}

// findIndexEntry returns the key of id in [lower, upper), whose keys are
// lower + <8-byte sequence or score> + "/" + <id>, or nil when there is
// none. The score is not recorded in the task body, so this scans the
// task's own bucket.
func (r *TaskRepository) findIndexEntry(lower, upper []byte, id string) ([]byte, error) {
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	idStart := len(lower) + scoreLen + 1
	for valid := it.First(); valid; valid = it.Next() {
		k := it.Key()
		if len(k) == idStart+len(id) && k[idStart-1] == '/' && string(k[idStart:]) == id {
			return bytes.Clone(k), nil
		}
	}
	return nil, it.Error()
}
