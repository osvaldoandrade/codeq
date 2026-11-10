package pebble

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"

	"github.com/osvaldoandrade/codeq/internal/metrics"
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

// EnsureLeaderDispatch is ensureLeaderDispatch for background work outside
// this package's request paths: it returns once this process leads, a
// barrier has applied every entry committed before its term, and the
// dispatch index has been rebuilt from that state. Standalone Pebble
// returns immediately.
func (r *TaskRepository) EnsureLeaderDispatch(ctx context.Context) error {
	return r.ensureLeaderDispatch(ctx)
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

// rebuildWarnEvery rate-limits the warning of a failed election-time
// rebuild; claims and reaper ticks retry the rebuild meanwhile.
const rebuildWarnEvery = 5 * time.Minute

// watchLeadership rebuilds the dispatch index on each election win and
// drops it on each loss. The goroutine exits when LeaderObservation is
// closed, which raft.DB.Close does by closing stopCh.
func (r *TaskRepository) watchLeadership(src leadershipSource) {
	var lastWarn time.Time
	// The rebuild reads Pebble outside any request; runOpen keeps it from
	// racing DB.Close during shutdown.
	rebuild := func() bool {
		return r.db.runOpen(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := r.ensureLeaderDispatch(ctx)
			cancel()
			if warnRebuildFailure(err, time.Now(), &lastWarn) {
				slog.Warn("codeq: election-time dispatch rebuild failed; claims and reaper sweeps wait for it and retry",
					"event", "codeq_dispatch_rebuild_failed", "errorClass", dispatchErrorClass(err))
			}
		})
	}
	if src.IsLeader() && !rebuild() {
		return
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
		if !rebuild() {
			return
		}
	}
}

// warnRebuildFailure reports whether a failed election-time rebuild should
// be logged now: not for success or a lost leadership, and at most once per
// rebuildWarnEvery. Only the watcher goroutine uses lastWarn.
func warnRebuildFailure(err error, now time.Time, lastWarn *time.Time) bool {
	var notLeader *NotLeaderError
	if err == nil || errors.As(err, &notLeader) {
		return false
	}
	if !lastWarn.IsZero() && now.Sub(*lastWarn) < rebuildWarnEvery {
		return false
	}
	*lastWarn = now
	return true
}

// Fixed classes of a leader-readiness failure; logs carry these instead of
// the error text.
const (
	errClassNone      = "none"
	errClassNotLeader = "not_leader"
	errClassTimeout   = "timeout"
	errClassCanceled  = "canceled"
	errClassOther     = "barrier_or_storage"
)

// dispatchErrorClass is the fixed class of a leader-readiness failure.
func dispatchErrorClass(err error) string {
	var notLeader *NotLeaderError
	switch {
	case err == nil:
		return errClassNone
	case errors.As(err, &notLeader), errors.Is(err, ErrNotLeader):
		return errClassNotLeader
	case errors.Is(err, context.DeadlineExceeded):
		return errClassTimeout
	case errors.Is(err, context.Canceled):
		return errClassCanceled
	default:
		return errClassOther
	}
}

// rebuildLocked replaces process-local dispatch state with a scan of
// the replicated Pebble keyspace. Caller holds dispatchMu.
func (r *TaskRepository) rebuildLocked() error {
	defer r.noteFootprintLocked()
	r.drainHintsLocked()
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

// drainHintsLocked drops every hint queue and overflow mark. The rebuild
// that follows scans the pending keys back. Caller holds dispatchMu.
func (r *TaskRepository) drainHintsLocked() {
	clear(r.queues)
	clear(r.overflow)
	r.queued = make(map[string]struct{})
}

func (r *TaskRepository) resetDelayedLocked() {
	r.delayedCount.Range(func(_, v any) bool {
		v.(*atomic.Int64).Store(0)
		return true
	})
}

// sendHintLocked publishes one hint. The queue of (cmd, tenant, prio) is
// created on its first hint. It reports false when the hint was not queued:
// the queue holds hintCap hints or is already overflowed. Either way the
// queue is (or stays) overflowed, so its pending keys, this one included,
// are refilled in key order once it drains. Caller holds dispatchMu
// (NewTaskRepository's recovery runs before any other goroutine can see the
// repository).
func (r *TaskRepository) sendHintLocked(cmd domain.Command, tenantID string, prio int, seq uint64, id string) bool {
	if _, ok := r.inflight[id]; ok {
		return true
	}
	if _, ok := r.queued[id]; ok {
		return true
	}
	key := queueKey(cmd, tenantID, prio)
	defer r.noteFootprintLocked()
	if _, over := r.overflow[key]; over {
		return false
	}
	if !r.pushHintLocked(key, seq, id) {
		r.overflow[key] = overflowQueue{cmd: cmd, tenantID: tenantID, prio: prio}
		return false
	}
	return true
}

// pushHintLocked appends one hint to the queue of key unless it holds
// hintCap hints. Caller holds dispatchMu and has checked the dedupe sets.
func (r *TaskRepository) pushHintLocked(key string, seq uint64, id string) bool {
	q := r.queues[key]
	if q == nil {
		q = &hintQueue{}
		r.queues[key] = q
	}
	if q.len() >= r.hintCap {
		return false
	}
	q.push(pendingHint{seq: seq, id: id})
	r.queued[id] = struct{}{}
	r.db.RaiseSeq(seq)
	return true
}

// refillLocked queues the oldest pending keys of an overflowed queue, up to
// hintCap, skipping tasks already queued or in flight, and reports whether
// it queued any. Claims call it only once the queue has drained, so the scan
// re-reads at most the in-flight keys before it finds new work: a bounded
// cost per hintCap claims. The queue leaves the overflow set when the scan
// reaches the end of its keys; a storage error keeps it there for the next
// claim. Caller holds dispatchMu.
func (r *TaskRepository) refillLocked(key string) bool {
	o, over := r.overflow[key]
	if !over {
		return false
	}
	lower, upper := PrefixPendingPrio(o.cmd, o.tenantID, o.prio)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return false
	}
	defer it.Close()
	added, full := 0, false
	for valid := it.First(); valid; valid = it.Next() {
		seq, id, ok := parsePendingSuffix(it.Key(), len(lower))
		if !ok {
			continue
		}
		if _, busy := r.inflight[id]; busy {
			continue
		}
		if _, queued := r.queued[id]; queued {
			continue
		}
		if !r.pushHintLocked(key, seq, id) {
			full = true
			break
		}
		added++
	}
	if it.Error() == nil && !full {
		delete(r.overflow, key)
	}
	return added > 0
}

// parsePendingSuffix reads <seq_be8>/<id> after a PrefixPendingPrio prefix
// of length prefixLen.
func parsePendingSuffix(k []byte, prefixLen int) (uint64, string, bool) {
	if len(k) < prefixLen+8+2 || k[prefixLen+8] != '/' {
		return 0, "", false
	}
	return beUint64(k[prefixLen : prefixLen+8]), string(k[prefixLen+9:]), true
}

func (r *TaskRepository) popHint(cmd domain.Command, tenantID string, prio int) (pendingHint, bool) {
	return r.popHintWithinBudget(context.Background(), cmd, tenantID, prio, nil)
}

// popHintWithinBudget takes the oldest hint of (cmd, tenant, prio) that is
// not already in flight. A nil budget preserves the batch-claim traversal.
// Single claims account for every removed hint, including a reserved task,
// before inspecting its body. A queue that drains is dropped, so an idle
// tuple holds no memory and a probe of an empty priority allocates nothing;
// an overflowed queue is refilled from Pebble when it is found empty.
func (r *TaskRepository) popHintWithinBudget(ctx context.Context, cmd domain.Command, tenantID string, prio int, remaining *int) (pendingHint, bool) {
	key := queueKey(cmd, tenantID, prio)
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	defer r.noteFootprintLocked()
	for remaining == nil || *remaining > 0 {
		if ctx.Err() != nil {
			return pendingHint{}, false
		}
		h, ok := r.nextHintLocked(key)
		if !ok {
			return pendingHint{}, false
		}
		if remaining != nil {
			*remaining--
		}
		delete(r.queued, h.id)
		if _, busy := r.inflight[h.id]; busy {
			continue
		}
		r.inflight[h.id] = struct{}{}
		return h, true
	}
	return pendingHint{}, false
}

// nextHintLocked removes the oldest hint of key, refilling an empty
// overflowed queue from Pebble first. A queue that drains is dropped. Caller
// holds dispatchMu.
func (r *TaskRepository) nextHintLocked(key string) (pendingHint, bool) {
	for {
		q := r.queues[key]
		if q == nil {
			if !r.refillLocked(key) {
				return pendingHint{}, false
			}
			continue
		}
		h, ok := q.pop()
		if q.len() == 0 {
			delete(r.queues, key)
		}
		if ok {
			return h, true
		}
	}
}

// DispatchFootprint is the leader-local claim index held in memory: hint
// queues, hints, in-flight claims and leases, plus the queues whose pending
// keys exceed their cap (Overflowed). Memory is proportional to these counts.
// It is sampled on a timer for metrics and memory-pressure logs.
type DispatchFootprint struct {
	Queues     int
	Hints      int
	InFlight   int
	Leases     int
	Overflowed int
}

// DispatchFootprint samples the dispatch index without taking dispatchMu, so
// the storage monitor never waits behind a rebuild or a refill; the counters
// are stored under that lock after every change.
func (r *TaskRepository) DispatchFootprint() DispatchFootprint {
	return DispatchFootprint{
		Queues:     int(r.footprint.queues.Load()),
		Hints:      int(r.footprint.hints.Load()),
		InFlight:   int(r.footprint.inflight.Load()),
		Leases:     r.leases.Len(),
		Overflowed: int(r.footprint.overflowed.Load()),
	}
}

// noteFootprintLocked stores the dispatch index sizes for DispatchFootprint.
// queued holds exactly the ids of the queued hints. Caller holds dispatchMu.
func (r *TaskRepository) noteFootprintLocked() {
	r.footprint.queues.Store(int64(len(r.queues)))
	r.footprint.hints.Store(int64(len(r.queued)))
	r.footprint.inflight.Store(int64(len(r.inflight)))
	r.footprint.overflowed.Store(int64(len(r.overflow)))
}

// ObserveDispatchMetrics publishes DispatchFootprint under the shard label
// and returns it. Sampled on the storage monitor's timer, never per request.
func (r *TaskRepository) ObserveDispatchMetrics(shard string) DispatchFootprint {
	f := r.DispatchFootprint()
	metrics.DispatchHintQueues.WithLabelValues(shard).Set(float64(f.Queues))
	metrics.DispatchHints.WithLabelValues(shard).Set(float64(f.Hints))
	metrics.DispatchInFlight.WithLabelValues(shard).Set(float64(f.InFlight))
	metrics.DispatchOverflowedQueues.WithLabelValues(shard).Set(float64(f.Overflowed))
	metrics.LeaseEntries.WithLabelValues(shard).Set(float64(f.Leases))
	return f
}

func (r *TaskRepository) finishHint(cmd domain.Command, tenantID string, prio int, h pendingHint, republish bool) {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	defer r.noteFootprintLocked()
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
