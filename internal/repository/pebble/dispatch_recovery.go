package pebble

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// leadershipSource is the piece of *raft.DB the dispatch index needs.
// It lives here so this package does not import raft.
type leadershipSource interface {
	IsLeader() bool
	LeaderObservation() <-chan bool
	LeadershipEpoch() uint64
	Barrier(ctx context.Context) error
}

// durableLease is the result of reading LeaseUntil off a replicated task
// body when the process-local lease table has no entry.
type durableLease int

const (
	durableLeaseMissing durableLease = iota
	durableLeaseLive
	durableLeaseExpired
)

// ensureLeaderDispatch makes this process the owner of the in-memory
// dispatch index before a write. Standalone Pebble returns immediately.
// A Raft follower returns NotLeaderError. A new leader waits until a
// barrier has applied the log and the pending/delayed/lease indexes
// have been rebuilt from that log.
func (r *TaskRepository) ensureLeaderDispatch(ctx context.Context) error {
	if r.db.repl == nil {
		return nil
	}
	for {
		done, err := r.dispatchPass(ctx)
		if done {
			return err
		}
	}
}

// dispatchPass either finishes the leadership check or waits for the
// goroutine already rebuilding. done is false only when the caller
// should retry after another rebuild completes.
func (r *TaskRepository) dispatchPass(ctx context.Context) (bool, error) {
	r.dispatchMu.Lock()
	if err := ctx.Err(); err != nil {
		r.dispatchMu.Unlock()
		return true, err
	}
	if !r.db.repl.IsLeader() {
		url := r.db.repl.LeaderHTTPAddr()
		r.dispatchMu.Unlock()
		return true, &NotLeaderError{LeaderURL: url}
	}
	if r.dispatchReady && r.rebuiltEpoch == r.leadershipEpoch() {
		r.dispatchMu.Unlock()
		return true, nil
	}
	if r.rebuilding {
		r.dispatchCond.Wait()
		r.dispatchMu.Unlock()
		return false, nil
	}
	r.rebuilding = true
	r.dispatchMu.Unlock()
	return true, r.completeRebuild(ctx)
}

func (r *TaskRepository) completeRebuild(ctx context.Context) error {
	err := r.barrier(ctx)
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	if err == nil && !r.db.repl.IsLeader() {
		err = &NotLeaderError{LeaderURL: r.db.repl.LeaderHTTPAddr()}
	}
	if err == nil {
		err = r.rebuildLocked()
	}
	if err == nil {
		r.rebuiltEpoch = r.leadershipEpoch()
		r.dispatchReady = true
	}
	r.rebuilding = false
	r.dispatchCond.Broadcast()
	return err
}

func (r *TaskRepository) leadershipEpoch() uint64 {
	src, ok := r.db.repl.(leadershipSource)
	if !ok {
		return 0
	}
	return src.LeadershipEpoch()
}

func (r *TaskRepository) barrier(ctx context.Context) error {
	src, ok := r.db.repl.(leadershipSource)
	if !ok {
		return nil
	}
	if err := src.Barrier(ctx); err != nil {
		if r.db.repl == nil || !r.db.repl.IsLeader() {
			url := ""
			if r.db.repl != nil {
				url = r.db.repl.LeaderHTTPAddr()
			}
			return &NotLeaderError{LeaderURL: url}
		}
		return err
	}
	return nil
}

// watchLeadership rebuilds the dispatch index on each election win and
// drops it on each loss. The goroutine exits when LeaderObservation is
// closed, which raft.DB.Close does by closing stopCh.
func (r *TaskRepository) watchLeadership(src leadershipSource) {
	if src.IsLeader() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = r.ensureLeaderDispatch(ctx)
		cancel()
	}
	for {
		leader, ok := <-src.LeaderObservation()
		if !ok {
			return
		}
		if !leader && !src.IsLeader() {
			r.dispatchMu.Lock()
			r.dispatchReady = false
			r.dispatchCond.Broadcast()
			r.dispatchMu.Unlock()
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = r.ensureLeaderDispatch(ctx)
		cancel()
	}
}

// rebuildLocked replaces process-local dispatch state with a scan of
// the replicated Pebble keyspace. Caller holds dispatchMu.
func (r *TaskRepository) rebuildLocked() error {
	r.drainHintsLocked()
	r.queued = make(map[string]struct{})
	if err := r.recoverQueues(); err != nil {
		return err
	}
	r.resetDelayedLocked()
	if err := r.recoverDelayedCounts(); err != nil {
		return err
	}
	r.leases.Clear()
	return r.recoverLeases()
}

func (r *TaskRepository) drainHintsLocked() {
	r.queues.Range(func(_, v any) bool {
		q := v.(*queueChan)
		for {
			select {
			case <-q.ch:
			default:
				return true
			}
		}
	})
}

func (r *TaskRepository) resetDelayedLocked() {
	r.delayedCount.Range(func(_, v any) bool {
		v.(*atomic.Int64).Store(0)
		return true
	})
}

func (r *TaskRepository) sendHintLocked(cmd domain.Command, tenantID string, prio int, seq uint64, id string) bool {
	if _, ok := r.inflight[id]; ok {
		return true
	}
	if _, ok := r.queued[id]; ok {
		return true
	}
	q := r.channelFor(cmd, tenantID, prio)
	select {
	case q.ch <- pendingHint{seq: seq, id: id}:
		r.queued[id] = struct{}{}
		r.db.RaiseSeq(seq)
		return true
	default:
		return false
	}
}

func (r *TaskRepository) popHint(q *queueChan) (pendingHint, bool) {
	return r.popHintWithinBudget(context.Background(), q, nil)
}

// A nil budget preserves the batch-claim traversal. Single claims account for
// every removed hint, including a reserved task, before inspecting its body.
func (r *TaskRepository) popHintWithinBudget(ctx context.Context, q *queueChan, remaining *int) (pendingHint, bool) {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	for remaining == nil || *remaining > 0 {
		if ctx.Err() != nil {
			return pendingHint{}, false
		}
		select {
		case h := <-q.ch:
			if remaining != nil {
				*remaining--
			}
			delete(r.queued, h.id)
			if _, busy := r.inflight[h.id]; busy {
				continue
			}
			r.inflight[h.id] = struct{}{}
			return h, true
		default:
			return pendingHint{}, false
		}
	}
	return pendingHint{}, false
}

func (r *TaskRepository) finishHint(cmd domain.Command, tenantID string, prio int, h pendingHint, republish bool) {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	delete(r.inflight, h.id)
	if republish {
		r.sendHintLocked(cmd, tenantID, prio, h.seq, h.id)
	}
}

// NoteDelayed records one replicated delayed key in the process-local
// counter. The reaper and Nack both land here so a leadership rebuild,
// which sets the counter from a disk scan, cannot interleave with the
// increment.
func (r *TaskRepository) NoteDelayed(cmd domain.Command, tenantID string) {
	r.addDelayed(cmd, tenantID, 1)
}

func (r *TaskRepository) addDelayed(cmd domain.Command, tenantID string, delta int64) {
	r.dispatchMu.Lock()
	c := r.delayedCounter(cmd, tenantID)
	if n := c.Add(delta); n < 0 {
		c.Store(0)
	}
	r.dispatchMu.Unlock()
}

func (r *TaskRepository) zeroDelayedIfEmpty(cmd domain.Command, tenantID string) {
	if r.hasDelayed(cmd, tenantID) {
		return
	}
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	if r.hasDelayed(cmd, tenantID) {
		return
	}
	k := cmdSeg(cmd) + "\x00" + tenantSeg(tenantID)
	if v, ok := r.delayedCount.Load(k); ok {
		v.(*atomic.Int64).Store(0)
	}
}

func (r *TaskRepository) hasDelayed(cmd domain.Command, tenantID string) bool {
	lower, upper := PrefixDelayed(cmd, tenantID)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return true
	}
	defer it.Close()
	return it.First()
}

func (r *TaskRepository) rememberDurableLease(id string, now time.Time) durableLease {
	taskJSON, err := r.db.Get(KeyTask(id))
	if err != nil {
		return durableLeaseMissing
	}
	var t domain.Task
	if err := sonic.Unmarshal(taskJSON, &t); err != nil || t.Status != domain.StatusInProgress {
		return durableLeaseMissing
	}
	if t.LeaseUntil == "" || t.WorkerID == "" {
		return durableLeaseExpired
	}
	until, err := time.Parse(time.RFC3339, t.LeaseUntil)
	if err != nil {
		return durableLeaseExpired
	}
	r.leases.Set(id, t.WorkerID, t.Command, t.TenantID, until.Unix())
	if until.Unix() > now.Unix() {
		return durableLeaseLive
	}
	return durableLeaseExpired
}
