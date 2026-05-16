package pebble

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"

	"github.com/osvaldoandrade/codeq/internal/backoff"
	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// taskRepo is the Pebble-backed implementation of repository.TaskRepository.
// Hot-path operations build a single pebble.Batch so all writes for one
// logical action land atomically. Reads bypass batching (Pebble's MVCC
// snapshot semantics already give us point-in-time consistency).
//
// Concurrency: pebble.DB is goroutine-safe. The only mutable state owned
// here besides the underlying DB is the reconcileTracker (atomic CAS) and
// the seq counter on DB itself (atomic add).
type TaskRepository struct {
	db                 *DB
	tz                 *time.Location
	backoffPolicy      string
	backoffBaseSeconds int
	backoffMaxSeconds  int

	reconcile reconcileTracker

	// queues is the lock-free claim fast path. Each (cmd, tenant, prio)
	// gets a buffered channel of task IDs; producers enqueue to Pebble
	// AND push the id onto the channel, consumers receive from the
	// channel and then commit the claim batch. Pebble's internal write
	// lock still serializes batches, but Go channel send/receive is
	// essentially free — multiple consumers can race for receives
	// without any user-space mutex.
	//
	// Crash recovery: on Open we scan every pending key under
	// codeq/q/.../pending/* and re-seed the matching channel, so the
	// fast path is never out of sync with the durable state after a
	// restart. Channel buffer is sized large enough (per-queue cap) that
	// producers rarely block; if they do, the send is context-cancellable.
	queues sync.Map // key string → *queueChan

	// delayedCount tracks the count of delayed entries per (cmd, tenant).
	// Incremented when Enqueue/Nack writes to a delayed bucket and
	// decremented by moveDueDelayedForTenant after each successful sweep.
	// moveDueDelayedForTenant uses this as a fast-path skip: counter==0
	// → return without opening the Pebble iter. Under steady-state load
	// most claims have no delayed work to do, and the iter was responsible
	// for ~27% of heap allocations in the Phase 0 profile.
	delayedCount sync.Map // key string (cmd \x00 tenant) → *atomic.Int64

	// delayedMoveFlag single-flights moveDueDelayedForTenant per (cmd,
	// tenant) via CAS, NOT a mutex. Without it, two concurrent Claims
	// can read the same delayed range, both write a new KeyPending for
	// every id, both publish a hint — and the same task ends up
	// double-claimed by two workers. With it, only the first goroutine
	// to flip the flag actually sweeps; losers skip the move and fall
	// through to the channel pop. The active sweeper publishes hints
	// that the losers can consume, so throughput is preserved.
	delayedMoveFlag sync.Map // key string (cmd \x00 tenant) → *atomic.Int32
}

// queueChan is the per-queue channel + recovery state. Each hint carries
// both the seq we used at Enqueue time and the id, so the consumer can
// rebuild the exact pending key (KeyPending uses (cmd, tenant, prio,
// seq, id)) and delete it in one shot — no range scan required.
type queueChan struct {
	ch chan pendingHint
}

// pendingHint travels through the fast-path channel between a producer
// (Enqueue / Abandon / MoveDueDelayed) and a consumer (Claim).
type pendingHint struct {
	seq uint64
	id  string
}

// channelBufferSize bounds how many task IDs we can stage in memory per
// queue before producers must wait for a consumer. 100k @ ~24 bytes/id
// caps memory at ~2.4 MiB per (cmd,tenant,prio) tuple — generous given
// typical workloads have a handful of active tuples.
const channelBufferSize = 100_000

const (
	minPriority         = 0
	maxPriority         = 9
	taskRetention       = 24 * time.Hour
	defaultInspectLimit = 200

	// defaultReconcileInterval matches the redis-backed repo; lease-expiry
	// repair runs at most once per interval per (cmd, tenant). On the Pebble
	// path the cost difference is smaller (no network) but we keep the same
	// behavior so semantics are uniform.
	defaultReconcileInterval = 500 * time.Millisecond
)

// reconcileTracker mirrors the Redis-side implementation: atomic CAS on a
// per-(cmd,tenant) last-run timestamp.
type reconcileTracker struct {
	interval time.Duration
	last     sync.Map // key: cmd + "\x00" + tenantID → *int64
}

func (rt *reconcileTracker) shouldRun(cmd domain.Command, tenantID string) bool {
	if rt.interval <= 0 {
		return true
	}
	key := string(cmd) + "\x00" + tenantID
	now := time.Now().UnixNano()
	threshold := now - int64(rt.interval)
	raw, _ := rt.last.LoadOrStore(key, new(int64))
	ptr := raw.(*int64)
	last := atomic.LoadInt64(ptr)
	if last > threshold {
		return false
	}
	return atomic.CompareAndSwapInt64(ptr, last, now)
}

// NewTaskRepository wires an open Pebble DB into the TaskRepository contract.
// tz is used for the "now" timestamps embedded in task JSON.
func NewTaskRepository(db *DB, tz *time.Location, backoffPolicy string, backoffBaseSeconds, backoffMaxSeconds int) *TaskRepository {
	if backoffBaseSeconds <= 0 {
		backoffBaseSeconds = 5
	}
	if backoffMaxSeconds <= 0 {
		backoffMaxSeconds = 900
	}
	if backoffPolicy == "" {
		backoffPolicy = "exp_full_jitter"
	}
	r := &TaskRepository{
		db:                 db,
		tz:                 tz,
		backoffPolicy:      backoffPolicy,
		backoffBaseSeconds: backoffBaseSeconds,
		backoffMaxSeconds:  backoffMaxSeconds,
		reconcile:          reconcileTracker{interval: defaultReconcileInterval},
	}
	// Re-seed in-memory queue channels from any pending keys that
	// survived a previous shutdown. Has to happen before any handler
	// starts serving claims; that's why we do it in the constructor.
	if err := r.recoverQueues(); err != nil {
		panic(fmt.Sprintf("pebble queue recovery: %v", err))
	}
	if err := r.recoverDelayedCounts(); err != nil {
		panic(fmt.Sprintf("pebble delayed-count recovery: %v", err))
	}
	return r
}

func (r *TaskRepository) now() time.Time { return time.Now().In(r.tz) }

func normalizePriority(p int) int {
	if p < minPriority {
		return minPriority
	}
	if p > maxPriority {
		return maxPriority
	}
	return p
}

// ---------- Enqueue ----------

func (r *TaskRepository) Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, error) {
	task, _, err := r.EnqueueWithReady(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, visibleAt, tenantID)
	return task, err
}

func (r *TaskRepository) EnqueueWithReady(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, bool, error) {
	return r.EnqueueWithID(ctx, uuid.NewString(), cmd, payload, priority, webhook, maxAttempts, idempotencyKey, visibleAt, tenantID)
}

// EnqueueWithID is the cluster-aware variant: callers (cluster.Router) pre-pick
// the task ID at the routing boundary so the (id → owner shard) mapping
// resolved by the consistent-hash ring is honoured. Local callers can pass
// uuid.NewString() and get the original semantics.
func (r *TaskRepository) EnqueueWithID(ctx context.Context, id string, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, bool, error) {
	if idempotencyKey != "" {
		// Look up existing task for this idempotency key. If present, return
		// the original task (mirrors the Redis behavior so SDKs see the same
		// idempotent contract regardless of backend).
		if existing, err := r.db.Get(KeyIdempo(idempotencyKey)); err == nil {
			existingID := string(existing)
			task, ferr := r.Get(ctx, existingID)
			if ferr == nil {
				return task, false, nil
			}
			// Idempo points to a deleted task: fall through and recreate.
		} else if !errors.Is(err, ErrNotFound) {
			return nil, false, fmt.Errorf("idempo lookup: %w", err)
		}
	}

	priority = normalizePriority(priority)
	now := r.now()
	task := &domain.Task{
		ID:          id,
		Command:     cmd,
		Payload:     payload,
		Priority:    priority,
		Webhook:     webhook,
		MaxAttempts: maxAttempts,
		Status:      domain.StatusPending,
		TenantID:    tenantID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	ready := false
	delayed := !visibleAt.IsZero() && visibleAt.After(now)
	if delayed {
		task.LastKnownLocation = domain.LocationDelayed
	} else {
		task.LastKnownLocation = domain.LocationPending
		ready = true
	}

	taskJSON, _ := sonic.Marshal(task)

	b := r.db.Batch()
	defer b.Close()

	// Persist task body.
	if err := b.Set(KeyTask(id), taskJSON, nil); err != nil {
		return nil, false, err
	}
	// TTL index (used by CleanupExpired reaper).
	ttlScore := uint64(now.Add(taskRetention).Unix())
	if err := b.Set(KeyTTLIndex(ttlScore, id), nil, nil); err != nil {
		return nil, false, err
	}
	// Pending vs delayed bucket.
	var pendingSeq uint64
	if delayed {
		score := uint64(visibleAt.Unix())
		if err := b.Set(KeyDelayed(cmd, tenantID, score, id), nil, nil); err != nil {
			return nil, false, err
		}
	} else {
		pendingSeq = r.db.NextSeq()
		if err := b.Set(KeyPending(cmd, tenantID, priority, pendingSeq, id), nil, nil); err != nil {
			return nil, false, err
		}
	}
	// Idempotency mapping last so a successful commit makes the whole tuple visible.
	if idempotencyKey != "" {
		if err := b.Set(KeyIdempo(idempotencyKey), []byte(id), nil); err != nil {
			return nil, false, err
		}
	}

	if err := r.db.CommitBatch(b); err != nil {
		return nil, false, fmt.Errorf("commit enqueue: %w", err)
	}
	// Publish the ID on the fast-path channel only after the durable
	// batch is committed. If the channel happens to be full (cap reached)
	// we drop the hint — the pending key is durable in Pebble, and any
	// reaper or restart will rediscover it. A drop here is a perf
	// regression (claim falls back to a scan), not a correctness one.
	if !delayed {
		r.publishPending(cmd, tenantID, priority, pendingSeq, id)
	} else {
		r.delayedCounter(cmd, tenantID).Add(1)
	}
	metrics.TaskCreatedTotal.WithLabelValues(string(cmd)).Inc()
	return task, ready, nil
}

// publishPending pushes a (seq, id) hint onto the per-queue channel
// non-blocking. The fall-through (channel full) is intentional: the
// data is already in Pebble; recovery on next restart picks it up.
func (r *TaskRepository) publishPending(cmd domain.Command, tenantID string, prio int, seq uint64, id string) {
	q := r.channelFor(cmd, tenantID, prio)
	select {
	case q.ch <- pendingHint{seq: seq, id: id}:
	default:
	}
}

// ---------- Get ----------

func (r *TaskRepository) Get(ctx context.Context, taskID string) (*domain.Task, error) {
	v, err := r.db.Get(KeyTask(taskID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("not-found")
		}
		return nil, err
	}
	var t domain.Task
	if err := sonic.Unmarshal(v, &t); err != nil {
		return nil, fmt.Errorf("unmarshal task: %w", err)
	}
	return &t, nil
}

// ---------- Claim ----------

func (r *TaskRepository) Claim(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, maxAttemptsDefault int, tenantID string) (*domain.Task, bool, error) {
	if inspectLimit <= 0 {
		inspectLimit = defaultInspectLimit
	}

	// Delayed→pending sweep on the hot path so Nack(delaySeconds=0) becomes
	// immediately claimable. Lease-expiry repair is throttled. We pass the
	// claimer's tenant explicitly so the iter scope (and the delayed-count
	// fast-path skip) matches the bucket the producer-side Nack/Enqueue
	// actually wrote to — the public MoveDueDelayed entry point still
	// defaults to tenant="" for admin/test paths.
	for _, cmd := range commands {
		if _, err := r.moveDueDelayedForTenant(ctx, cmd, inspectLimit, tenantID); err != nil {
			return nil, false, err
		}
		if r.reconcile.shouldRun(cmd, tenantID) {
			if _, err := r.requeueExpired(ctx, cmd, inspectLimit, maxAttemptsDefault, tenantID); err != nil {
				return nil, false, err
			}
		}
	}

	for _, cmd := range commands {
		t, ok, err := r.popFromAnyPriority(ctx, workerID, cmd, leaseSeconds, tenantID)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return t, true, nil
		}
	}
	return nil, false, nil
}

// popFromAnyPriority walks priorities high→low. For each priority it grabs
// the lowest-seq pending key (FIFO within bucket), atomically deletes it,
// adds the id to the inprog set, updates the task body, and writes the
// lease — all in one batch so we never observe a half-moved task.
func (r *TaskRepository) popFromAnyPriority(ctx context.Context, workerID string, cmd domain.Command, leaseSeconds int, tenantID string) (*domain.Task, bool, error) {
	for p := maxPriority; p >= minPriority; p-- {
		t, ok, err := r.tryPopPriority(ctx, workerID, cmd, p, leaseSeconds, tenantID)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return t, true, nil
		}
	}
	return nil, false, nil
}

// queueKey is the stable string identifier used to look up the per-queue
// channel. Uses the same normalization as the on-disk keys (cmdSeg
// lowercases, tenantSeg maps "" → "_") so that recoverQueues — which
// parses the (cmd, tenant) tuple out of disk keys — and producer-side
// channelFor calls coming in with the raw API cmd resolve to the same
// channel. Without this, recovered hints land on a different map entry
// from the one Claim later reads from, stranding the IDs.
func queueKey(cmd domain.Command, tenantID string, prio int) string {
	return cmdSeg(cmd) + "\x00" + tenantSeg(tenantID) + "\x00" + strconv.Itoa(prio)
}

// channelFor returns the buffered ID channel for a (cmd, tenant, prio).
// LoadOrStore makes the lazy creation safe for concurrent producers and
// consumers; whichever wins the store gets credited, the loser uses the
// stored value. New channels start empty (any pre-existing pending keys
// were seeded by recoverQueues at startup).
func (r *TaskRepository) channelFor(cmd domain.Command, tenantID string, prio int) *queueChan {
	k := queueKey(cmd, tenantID, prio)
	if v, ok := r.queues.Load(k); ok {
		return v.(*queueChan)
	}
	q := &queueChan{ch: make(chan pendingHint, channelBufferSize)}
	actual, _ := r.queues.LoadOrStore(k, q)
	return actual.(*queueChan)
}

// delayedMoveFlagFor returns the per-(cmd, tenantID) CAS flag used to
// single-flight moveDueDelayedForTenant. Same normalization rules as
// delayedCounter / channelFor so concurrent callers converge on the
// same atomic.
func (r *TaskRepository) delayedMoveFlagFor(cmd domain.Command, tenantID string) *atomic.Int32 {
	k := cmdSeg(cmd) + "\x00" + tenantSeg(tenantID)
	if v, ok := r.delayedMoveFlag.Load(k); ok {
		return v.(*atomic.Int32)
	}
	f := new(atomic.Int32)
	actual, _ := r.delayedMoveFlag.LoadOrStore(k, f)
	return actual.(*atomic.Int32)
}

// delayedCounter returns the atomic counter of delayed entries for
// (cmd, tenantID). Lazy initialized; safe for concurrent producers.
// Normalizes the lookup key via cmdSeg/tenantSeg so producer-side
// increments (called with raw API cmd) and recoverDelayedCounts
// (which parses cmd from the lowercased on-disk key) hit the same
// counter object.
func (r *TaskRepository) delayedCounter(cmd domain.Command, tenantID string) *atomic.Int64 {
	k := cmdSeg(cmd) + "\x00" + tenantSeg(tenantID)
	if v, ok := r.delayedCount.Load(k); ok {
		return v.(*atomic.Int64)
	}
	n := new(atomic.Int64)
	actual, _ := r.delayedCount.LoadOrStore(k, n)
	return actual.(*atomic.Int64)
}

// recoverQueues scans every pending key under codeq/q/.../pending/* at
// startup and re-seeds the matching channel. Without this, IDs that were
// durably enqueued before a restart would never be claimed (the channel
// would stay empty until new producers pushed).
//
// Runs once during NewTaskRepository so the workload is paid up-front
// rather than spread across the first batch of claims. Linear in the
// number of pending keys; for very deep queues this could be made
// concurrent or chunked.
func (r *TaskRepository) recoverQueues() error {
	lower := []byte(pQueue)
	upper := prefixUpper(lower)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return err
	}
	defer it.Close()

	pendingMarker := []byte(segPending)
	for valid := it.First(); valid; valid = it.Next() {
		k := it.Key()
		idx := indexOf(k, pendingMarker)
		if idx < 0 {
			continue
		}
		// key = .../pending/<prio_be1>/<seq_be8>/<id>
		// Extract cmd + tenant from the segment between pQueue and segPending.
		prefix := k[len(pQueue):idx]
		parts := splitN(prefix, '/', 2)
		if len(parts) != 2 {
			continue
		}
		cmd := domain.Command(string(parts[0]))
		tenantID := string(parts[1])
		if tenantID == emptyTenant {
			tenantID = ""
		}
		// Priority is the single byte right after segPending; seq is the
		// next 8 bytes (big-endian).
		pStart := idx + len(pendingMarker)
		if pStart+1+8 > len(k) {
			continue
		}
		prio := int(k[pStart])
		seq := beUint64(k[pStart+1+1 : pStart+1+1+8])
		id, ok := ParsePendingKey(k)
		if !ok {
			continue
		}
		// Non-blocking send: if the channel was sized too small (shouldn't
		// happen given the cap) we'd drop the recovery hint. Drop is safe
		// because the pending key stays in Pebble — a later scan/restart
		// would pick it up. We log loudly if it happens.
		q := r.channelFor(cmd, tenantID, prio)
		select {
		case q.ch <- pendingHint{seq: seq, id: id}:
		default:
			return fmt.Errorf("channel full during recovery for queue %s/%s/%d (raise channelBufferSize)", cmd, tenantID, prio)
		}
	}
	return it.Error()
}

// recoverDelayedCounts seeds the delayed-entry counter map by walking
// every delayed key once at startup. Parses the (cmd, tenant) tuple out
// of each key and bumps the matching counter. Without this, a restart
// would zero the counters and Claim's fast-path skip would let real
// delayed tasks sit indefinitely until a new Nack or Enqueue bumped
// the counter back above zero.
func (r *TaskRepository) recoverDelayedCounts() error {
	lower := []byte(pQueue)
	upper := prefixUpper(lower)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return err
	}
	defer it.Close()

	delayedMarker := []byte(segDelayed)
	for valid := it.First(); valid; valid = it.Next() {
		k := it.Key()
		idx := indexOf(k, delayedMarker)
		if idx < 0 {
			continue
		}
		prefix := k[len(pQueue):idx]
		parts := splitN(prefix, '/', 2)
		if len(parts) != 2 {
			continue
		}
		cmd := domain.Command(string(parts[0]))
		tenantID := string(parts[1])
		if tenantID == emptyTenant {
			tenantID = ""
		}
		r.delayedCounter(cmd, tenantID).Add(1)
	}
	return it.Error()
}

func (r *TaskRepository) tryPopPriority(ctx context.Context, workerID string, cmd domain.Command, priority, leaseSeconds int, tenantID string) (*domain.Task, bool, error) {
	q := r.channelFor(cmd, tenantID, priority)
	var h pendingHint
	select {
	case h = <-q.ch:
	default:
		return nil, false, nil
	}
	return r.completeClaim(ctx, workerID, cmd, tenantID, priority, leaseSeconds, h.seq, h.id)
}

// completeClaim performs the durable side of a claim. Because the channel
// hint carries the seq we used at enqueue time, we can rebuild the exact
// pending key with KeyPending — no iterator scan. The hot path is now:
// 1 Get (task body) + 1 batch commit (5 ops). Pebble's internal write
// lock serializes the commits but at microsecond cost.
//
// Ghosts: if the task body is missing (admin cleanup raced), we drop the
// matching pending key by exact key — no scan — and signal empty.
func (r *TaskRepository) completeClaim(ctx context.Context, workerID string, cmd domain.Command, tenantID string, priority, leaseSeconds int, seq uint64, id string) (*domain.Task, bool, error) {
	pendingKey := KeyPending(cmd, tenantID, priority, seq, id)

	taskJSON, err := r.db.Get(KeyTask(id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			_ = r.db.Delete(pendingKey)
			return nil, false, nil
		}
		return nil, false, err
	}
	var t domain.Task
	if err := sonic.Unmarshal(taskJSON, &t); err != nil {
		_ = r.db.Delete(pendingKey)
		return nil, false, nil
	}

	// Defense in depth: only PENDING tasks may transition to INPROG.
	// If we got here with the task already INPROG / COMPLETED / FAILED,
	// a duplicate pending hint reached the channel (recovery race or a
	// missed single-flight in moveDueDelayedForTenant). Drop the stale
	// pending key and bail — the rightful claimer is already at work
	// and we must not steal the worker assignment.
	if t.Status != domain.StatusPending {
		_ = r.db.Delete(pendingKey)
		return nil, false, nil
	}

	now := r.now()
	leaseUntil := now.Add(time.Duration(leaseSeconds) * time.Second).UTC()
	t.Status = domain.StatusInProgress
	t.LastKnownLocation = domain.LocationInProgress
	t.WorkerID = workerID
	t.LeaseUntil = leaseUntil.Format(time.RFC3339)
	t.Attempts++
	t.UpdatedAt = now
	updatedJSON, _ := sonic.Marshal(&t)

	b := r.db.Batch()
	defer b.Close()
	if err := b.Delete(pendingKey, nil); err != nil {
		return nil, false, err
	}
	if err := b.Set(KeyInprog(cmd, tenantID, id), nil, nil); err != nil {
		return nil, false, err
	}
	if err := b.Set(KeyTask(id), updatedJSON, nil); err != nil {
		return nil, false, err
	}
	if err := b.Set(KeyLease(id), encodeLease(workerID, leaseUntil), nil); err != nil {
		return nil, false, err
	}
	ttlScore := uint64(now.Add(taskRetention).Unix())
	if err := b.Set(KeyTTLIndex(ttlScore, id), nil, nil); err != nil {
		return nil, false, err
	}
	if err := r.db.CommitBatch(b); err != nil {
		return nil, false, fmt.Errorf("commit claim: %w", err)
	}
	return &t, true, nil
}

// findPendingKey scans the per-prio pending prefix looking for an entry
// whose trailing id matches. The seq we wrote at Enqueue time isn't
// carried through the channel hint, so we have to discover it here. The
// scan is bounded by the few entries that happen to share this id —
// normally exactly one.
func (r *TaskRepository) findPendingKey(cmd domain.Command, tenantID string, priority int, id string) ([]byte, bool) {
	lower, upper := PrefixPendingPrio(cmd, tenantID, priority)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return nil, false
	}
	defer it.Close()
	idBytes := []byte(id)
	for valid := it.First(); valid; valid = it.Next() {
		k := it.Key()
		if bytesHasSuffix(k, idBytes) {
			out := append([]byte(nil), k...)
			return out, true
		}
	}
	return nil, false
}

// dropPendingByID deletes any pending entry for id within (cmd, tenant,
// prio) — used to clean up ghost references discovered during claim.
func (r *TaskRepository) dropPendingByID(cmd domain.Command, tenantID string, priority int, id string) {
	if k, ok := r.findPendingKey(cmd, tenantID, priority, id); ok {
		_ = r.db.Delete(k)
	}
}

// indexOf is a small helper that returns the first index of sub within s,
// or -1. We have bytes.Index in the std library but inlining keeps this
// hot path free of import drift.
func indexOf(s, sub []byte) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := range sub {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// splitN splits s on sep returning at most n parts (the rest joined). Used
// at startup to parse "cmd/tenant" out of a queue key prefix.
func splitN(s []byte, sep byte, n int) [][]byte {
	out := make([][]byte, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func bytesHasSuffix(s, suffix []byte) bool {
	return len(s) >= len(suffix) && string(s[len(s)-len(suffix):]) == string(suffix)
}

func encodeLease(workerID string, until time.Time) []byte {
	return []byte(workerID + "|" + strconv.FormatInt(until.Unix(), 10))
}

func parseLease(v []byte) (workerID string, untilUnix int64, ok bool) {
	s := string(v)
	idx := strings.IndexByte(s, '|')
	if idx < 0 {
		return "", 0, false
	}
	until, err := strconv.ParseInt(s[idx+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return s[:idx], until, true
}

// ---------- Heartbeat ----------

func (r *TaskRepository) Heartbeat(ctx context.Context, taskID string, workerID string, extendSeconds int) error {
	taskJSON, err := r.db.Get(KeyTask(taskID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("not-found")
		}
		return err
	}
	var t domain.Task
	if err := sonic.Unmarshal(taskJSON, &t); err != nil {
		return fmt.Errorf("unmarshal task: %w", err)
	}
	if t.WorkerID != workerID {
		return fmt.Errorf("not-owner")
	}

	now := r.now()
	until := now.Add(time.Duration(extendSeconds) * time.Second).UTC()
	t.LeaseUntil = until.Format(time.RFC3339)
	t.UpdatedAt = now
	t.LastKnownLocation = domain.LocationInProgress
	updated, _ := sonic.Marshal(&t)

	b := r.db.Batch()
	defer b.Close()
	if err := b.Set(KeyTask(taskID), updated, nil); err != nil {
		return err
	}
	if err := b.Set(KeyLease(taskID), encodeLease(workerID, until), nil); err != nil {
		return err
	}
	ttlScore := uint64(now.Add(taskRetention).Unix())
	if err := b.Set(KeyTTLIndex(ttlScore, taskID), nil, nil); err != nil {
		return err
	}
	return r.db.CommitBatch(b)
}

// ---------- Abandon ----------

func (r *TaskRepository) Abandon(ctx context.Context, taskID string, workerID string) error {
	taskJSON, err := r.db.Get(KeyTask(taskID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("not-found")
		}
		return err
	}
	var t domain.Task
	if err := sonic.Unmarshal(taskJSON, &t); err != nil {
		return fmt.Errorf("unmarshal task: %w", err)
	}
	if workerID != "" && t.WorkerID != workerID {
		return fmt.Errorf("not-owner")
	}
	if t.Status != domain.StatusInProgress {
		return fmt.Errorf("not-in-progress")
	}

	now := r.now()
	t.Status = domain.StatusPending
	t.LastKnownLocation = domain.LocationPending
	t.WorkerID = ""
	t.LeaseUntil = ""
	t.UpdatedAt = now
	updated, _ := sonic.Marshal(&t)
	prio := normalizePriority(t.Priority)
	seq := r.db.NextSeq()

	b := r.db.Batch()
	defer b.Close()
	if err := b.Delete(KeyInprog(t.Command, t.TenantID, taskID), nil); err != nil {
		return err
	}
	if err := b.Delete(KeyLease(taskID), nil); err != nil {
		return err
	}
	if err := b.Set(KeyPending(t.Command, t.TenantID, prio, seq, taskID), nil, nil); err != nil {
		return err
	}
	if err := b.Set(KeyTask(taskID), updated, nil); err != nil {
		return err
	}
	if err := r.db.CommitBatch(b); err != nil {
		return err
	}
	r.publishPending(t.Command, t.TenantID, prio, seq, taskID)
	return nil
}

// ---------- Nack ----------

func (r *TaskRepository) Nack(ctx context.Context, taskID string, workerID string, delaySeconds int, maxAttemptsDefault int, reason string) (int, bool, error) {
	taskJSON, err := r.db.Get(KeyTask(taskID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, false, fmt.Errorf("not-found")
		}
		return 0, false, err
	}
	var t domain.Task
	if err := sonic.Unmarshal(taskJSON, &t); err != nil {
		return 0, false, fmt.Errorf("unmarshal task: %w", err)
	}
	if workerID != "" && t.WorkerID != workerID {
		return 0, false, fmt.Errorf("not-owner")
	}
	if t.Status != domain.StatusInProgress {
		return 0, false, fmt.Errorf("not-in-progress")
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = maxAttemptsDefault
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = 1
	}
	t.Attempts++

	now := r.now()
	if t.Attempts >= t.MaxAttempts {
		if reason == "" {
			reason = "MAX_ATTEMPTS"
		}
		t.Status = domain.StatusFailed
		t.LastKnownLocation = domain.LocationDLQ
		t.WorkerID = ""
		t.LeaseUntil = ""
		t.Error = reason
		t.UpdatedAt = now
		updated, _ := sonic.Marshal(&t)

		b := r.db.Batch()
		defer b.Close()
		if err := b.Delete(KeyInprog(t.Command, t.TenantID, taskID), nil); err != nil {
			return 0, false, err
		}
		if err := b.Delete(KeyLease(taskID), nil); err != nil {
			return 0, false, err
		}
		if err := b.Set(KeyDLQ(t.Command, t.TenantID, taskID), nil, nil); err != nil {
			return 0, false, err
		}
		if err := b.Set(KeyTask(taskID), updated, nil); err != nil {
			return 0, false, err
		}
		if err := r.db.CommitBatch(b); err != nil {
			return 0, false, err
		}
		metrics.TaskCompletedTotal.WithLabelValues(string(t.Command), string(t.Status)).Inc()
		if d := now.Sub(t.CreatedAt).Seconds(); d >= 0 {
			metrics.TaskProcessingLatencySeconds.WithLabelValues(string(t.Command), string(t.Status)).Observe(d)
		}
		return 0, true, nil
	}

	if delaySeconds < 0 {
		delaySeconds = 0
	}
	visibleAt := now.Add(time.Duration(delaySeconds) * time.Second).UTC()
	t.Status = domain.StatusPending
	t.LastKnownLocation = domain.LocationDelayed
	t.WorkerID = ""
	t.LeaseUntil = ""
	t.Error = ""
	t.UpdatedAt = now
	updated, _ := sonic.Marshal(&t)

	b := r.db.Batch()
	defer b.Close()
	if err := b.Delete(KeyInprog(t.Command, t.TenantID, taskID), nil); err != nil {
		return 0, false, err
	}
	if err := b.Delete(KeyLease(taskID), nil); err != nil {
		return 0, false, err
	}
	score := uint64(visibleAt.Unix())
	if err := b.Set(KeyDelayed(t.Command, t.TenantID, score, taskID), nil, nil); err != nil {
		return 0, false, err
	}
	if err := b.Set(KeyTask(taskID), updated, nil); err != nil {
		return 0, false, err
	}
	if err := r.db.CommitBatch(b); err != nil {
		return 0, false, err
	}
	r.delayedCounter(t.Command, t.TenantID).Add(1)
	return delaySeconds, false, nil
}

// ---------- MoveDueDelayed ----------

func (r *TaskRepository) MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error) {
	return r.moveDueDelayedForTenant(ctx, cmd, limit, "")
}

func (r *TaskRepository) moveDueDelayedForTenant(ctx context.Context, cmd domain.Command, limit int, tenantID string) (int, error) {
	if limit <= 0 {
		limit = defaultInspectLimit
	}
	// Fast path: if no delayed entries exist for this (cmd, tenant), skip
	// the Pebble iter entirely. Phase 0 profile showed this iter accounted
	// for ~27% of heap allocs because Claim invokes it on every call even
	// when there's nothing to move.
	counter := r.delayedCounter(cmd, tenantID)
	if counter.Load() == 0 {
		return 0, nil
	}
	// Single-flight concurrent sweeps for the same (cmd, tenant). Two
	// workers hitting this in parallel would otherwise read the same
	// delayed range, each write a new KeyPending for every id with a
	// fresh seq, and each publish a hint on the queue channel — so the
	// same task gets claimed twice. Only the second claim wins the
	// in-progress bit, but BOTH workers think they own it, and the
	// loser's Submit then fails with 409 not-in-progress once the
	// first worker finalizes the task. The CAS-flag pattern (vs a
	// mutex) means concurrent Claim callers don't block waiting for
	// the sweep — they fall through to the channel pop and pick up
	// the hints the winning sweeper publishes.
	flag := r.delayedMoveFlagFor(cmd, tenantID)
	if !flag.CompareAndSwap(0, 1) {
		return 0, nil
	}
	defer flag.Store(0)
	now := r.now()
	lower, upper := PrefixDelayedUpTo(cmd, tenantID, uint64(now.Unix()))
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return 0, err
	}
	defer it.Close()

	// Collect first; mutate after to avoid invalidating the iterator.
	type entry struct {
		delayedKey []byte
		id         string
	}
	batchEntries := make([]entry, 0, limit)
	for valid := it.First(); valid && len(batchEntries) < limit; valid = it.Next() {
		k := append([]byte(nil), it.Key()...)
		id, ok := ParseDelayedKey(k)
		if !ok {
			continue
		}
		batchEntries = append(batchEntries, entry{delayedKey: k, id: id})
	}
	if len(batchEntries) == 0 {
		return 0, nil
	}

	b := r.db.Batch()
	defer b.Close()
	moved := 0
	// We need to publish each moved entry on the channel *after* commit
	// so consumers can't pick up an id whose pending key isn't durable yet.
	type publish struct {
		cmd      domain.Command
		tenantID string
		prio     int
		seq      uint64
		id       string
	}
	published := make([]publish, 0, len(batchEntries))
	for _, e := range batchEntries {
		taskJSON, err := r.db.Get(KeyTask(e.id))
		if err != nil {
			// Ghost delayed entry — drop and keep going.
			_ = b.Delete(e.delayedKey, nil)
			continue
		}
		var t domain.Task
		if err := sonic.Unmarshal(taskJSON, &t); err != nil {
			_ = b.Delete(e.delayedKey, nil)
			continue
		}
		prio := normalizePriority(t.Priority)
		seq := r.db.NextSeq()
		t.Status = domain.StatusPending
		t.LastKnownLocation = domain.LocationPending
		t.WorkerID = ""
		t.LeaseUntil = ""
		t.UpdatedAt = now
		updated, _ := sonic.Marshal(&t)
		if err := b.Delete(e.delayedKey, nil); err != nil {
			return moved, err
		}
		if err := b.Set(KeyPending(cmd, t.TenantID, prio, seq, e.id), nil, nil); err != nil {
			return moved, err
		}
		if err := b.Set(KeyTask(e.id), updated, nil); err != nil {
			return moved, err
		}
		published = append(published, publish{cmd: cmd, tenantID: t.TenantID, prio: prio, seq: seq, id: e.id})
		moved++
	}
	if err := r.db.CommitBatch(b); err != nil {
		return 0, err
	}
	// Every batchEntries item removed a key from the delayed bucket —
	// some via successful move (counted in `moved`), some via ghost
	// drop. Counter must track total deletions so it stays in sync
	// with the on-disk reality.
	counter.Add(-int64(len(batchEntries)))
	for _, p := range published {
		r.publishPending(p.cmd, p.tenantID, p.prio, p.seq, p.id)
	}
	return moved, nil
}

// ---------- requeueExpired ----------

func (r *TaskRepository) requeueExpired(ctx context.Context, cmd domain.Command, inspectLimit, maxAttemptsDefault int, tenantID string) (int, error) {
	if inspectLimit <= 0 {
		inspectLimit = defaultInspectLimit
	}
	lower, upper := PrefixInprog(cmd, tenantID)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return 0, err
	}
	defer it.Close()

	type entry struct {
		id     string
		inprog []byte
	}
	candidates := make([]entry, 0, inspectLimit)
	for valid := it.First(); valid && len(candidates) < inspectLimit; valid = it.Next() {
		k := append([]byte(nil), it.Key()...)
		// inprog/<id>
		idx := strings.LastIndexByte(string(k), '/')
		if idx < 0 || idx+1 >= len(k) {
			continue
		}
		candidates = append(candidates, entry{id: string(k[idx+1:]), inprog: k})
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	now := r.now()
	moved := 0
	for _, c := range candidates {
		leaseVal, err := r.db.Get(KeyLease(c.id))
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				return moved, err
			}
			// Lease gone but inprog member lingers — clean up.
			_ = r.db.Delete(c.inprog)
			continue
		}
		_, until, ok := parseLease(leaseVal)
		if !ok || until > now.Unix() {
			continue // still leased
		}

		// Lease expired — Nack into delayed with backoff so the existing
		// retry policy applies.
		taskJSON, err := r.db.Get(KeyTask(c.id))
		if err != nil {
			_ = r.db.Delete(c.inprog)
			continue
		}
		var t domain.Task
		if err := sonic.Unmarshal(taskJSON, &t); err != nil {
			_ = r.db.Delete(c.inprog)
			continue
		}
		metrics.LeaseExpiredTotal.WithLabelValues(string(cmd)).Inc()
		delaySeconds := backoff.Compute(r.backoffPolicy, r.backoffBaseSeconds, r.backoffMaxSeconds, t.Attempts, nil)
		// Re-use Nack to apply backoff/DLQ semantics — pass workerID="" so
		// the ownership check is skipped.
		if _, _, err := r.Nack(ctx, c.id, "", delaySeconds, maxAttemptsDefault, "LEASE_EXPIRED"); err != nil {
			msg := err.Error()
			if strings.Contains(msg, "not-in-progress") || strings.Contains(msg, "not-found") {
				_ = r.db.Delete(c.inprog)
				moved++
				continue
			}
			return moved, err
		}
		moved++
	}
	return moved, nil
}

// ---------- introspection ----------

func (r *TaskRepository) PendingLength(ctx context.Context, cmd domain.Command) (int64, error) {
	lower, upper := PrefixPendingAllPrios(cmd, "")
	return r.countPrefix(lower, upper)
}

func (r *TaskRepository) QueueStats(ctx context.Context, cmd domain.Command, tenantID string) (*domain.QueueStats, error) {
	pendingLow, pendingHigh := PrefixPendingAllPrios(cmd, tenantID)
	pending, err := r.countPrefix(pendingLow, pendingHigh)
	if err != nil {
		return nil, err
	}
	inprogLow, inprogHigh := PrefixInprog(cmd, tenantID)
	inprog, err := r.countPrefix(inprogLow, inprogHigh)
	if err != nil {
		return nil, err
	}
	delayedLow, delayedHigh := PrefixDelayed(cmd, tenantID)
	delayed, err := r.countPrefix(delayedLow, delayedHigh)
	if err != nil {
		return nil, err
	}
	dlqLow, dlqHigh := PrefixDLQ(cmd, tenantID)
	dlq, err := r.countPrefix(dlqLow, dlqHigh)
	if err != nil {
		return nil, err
	}
	return &domain.QueueStats{
		Command:    cmd,
		Ready:      pending,
		InProgress: inprog,
		Delayed:    delayed,
		DLQ:        dlq,
	}, nil
}

func (r *TaskRepository) AdminQueues(ctx context.Context) (map[string]any, error) {
	// Walk every queue prefix under codeq/q/ and aggregate counts. Output
	// shape mirrors the Redis flat-map for compatibility with the admin UI.
	lower := []byte(pQueue)
	upper := prefixUpper(lower)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	out := make(map[string]any)
	for valid := it.First(); valid; valid = it.Next() {
		k := string(it.Key())
		// Reduce to "<cmd>/<tenant>/<queue_type>" granularity for the count map.
		bucket := bucketLabel(k)
		if bucket == "" {
			continue
		}
		// First sighting of bucket: zero value of `any` is nil, so a
		// blind `.(int64)` panics. The Redis path produces flat int64
		// counts so we mirror that.
		prev, _ := out[bucket].(int64)
		out[bucket] = prev + 1
	}
	return out, nil
}

// bucketLabel converts a queue key into the same string the Redis backend
// publishes (e.g., "codeq:q:generate_master:pending:0"). Pending entries
// expose the priority; other queues collapse to type.
func bucketLabel(k string) string {
	// k = codeq/q/<cmd>/<tenant>/<type>/...
	if !strings.HasPrefix(k, pQueue) {
		return ""
	}
	rest := k[len(pQueue):]
	parts := strings.SplitN(rest, "/", 5)
	if len(parts) < 4 {
		return ""
	}
	cmd, tenant, typ := parts[0], parts[1], parts[2]
	label := "codeq:q:" + cmd
	if tenant != emptyTenant {
		label += ":" + tenant
	}
	switch typ {
	case "pending":
		if len(parts) >= 4 && len(parts[3]) >= 1 {
			label += ":pending:" + fmt.Sprintf("%d", int(parts[3][0]))
		}
	case "inprog", "delayed", "dlq":
		label += ":" + typ
	default:
		return ""
	}
	return label
}

func (r *TaskRepository) countPrefix(lower, upper []byte) (int64, error) {
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return 0, err
	}
	defer it.Close()
	var n int64
	for valid := it.First(); valid; valid = it.Next() {
		n++
	}
	return n, nil
}

// CleanupExpired sweeps the ttl_index for entries older than `before`,
// removing the task and any queue references. Bounded by `limit` per call.
func (r *TaskRepository) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	lower, _ := PrefixTTL()
	_, upperFull := PrefixTTL()
	// Restrict upper bound to keys with expire <= before.
	upper := make([]byte, 0, len(pTTL)+8)
	upper = append(upper, pTTL...)
	upper = append(upper, be8(uint64(before.Unix())+1)...)
	if string(upper) > string(upperFull) {
		upper = upperFull
	}
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return 0, err
	}
	defer it.Close()

	type cand struct {
		ttlKey []byte
		id     string
	}
	cands := make([]cand, 0, limit)
	for valid := it.First(); valid && len(cands) < limit; valid = it.Next() {
		k := append([]byte(nil), it.Key()...)
		// ttl/<be8>/<id>
		idx := strings.LastIndexByte(string(k), '/')
		if idx < 0 {
			continue
		}
		cands = append(cands, cand{ttlKey: k, id: string(k[idx+1:])})
	}
	if len(cands) == 0 {
		return 0, nil
	}

	b := r.db.Batch()
	defer b.Close()
	deleted := 0
	for _, c := range cands {
		// Remove the task body itself; queue references (if any) are
		// orphaned but reaped lazily by the ghost-detection path in Claim.
		if err := b.Delete(KeyTask(c.id), nil); err != nil {
			return deleted, err
		}
		if err := b.Delete(c.ttlKey, nil); err != nil {
			return deleted, err
		}
		if err := b.Delete(KeyLease(c.id), nil); err != nil {
			return deleted, err
		}
		deleted++
	}
	if err := r.db.CommitBatch(b); err != nil {
		return 0, err
	}
	return deleted, nil
}

// ParseDelayedKey extracts the id from a delayed key. Mirrors
// ParsePendingKey but documents the assumed layout.
func ParseDelayedKey(key []byte) (id string, ok bool) {
	idx := strings.LastIndexByte(string(key), '/')
	if idx < 0 || idx+1 >= len(key) {
		return "", false
	}
	return string(key[idx+1:]), true
}
