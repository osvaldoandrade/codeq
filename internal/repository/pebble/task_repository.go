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
	"github.com/osvaldoandrade/codeq/internal/repository"
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
type taskRepo struct {
	db                 *DB
	tz                 *time.Location
	backoffPolicy      string
	backoffBaseSeconds int
	backoffMaxSeconds  int

	reconcile reconcileTracker

	// queueLocks serializes claim attempts per (cmd, tenant, prio) so two
	// workers can't read the same pending key before either commits its
	// claim batch — Pebble batches don't have CAS, so without this lock
	// concurrent claimers can both "pop" the same id. Cost is a single
	// in-process mutex acquire; the protected critical section is one
	// iterator open + one batch commit (microseconds).
	queueLocks sync.Map // key string → *sync.Mutex
}

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
func NewTaskRepository(db *DB, tz *time.Location, backoffPolicy string, backoffBaseSeconds, backoffMaxSeconds int) repository.TaskRepository {
	if backoffBaseSeconds <= 0 {
		backoffBaseSeconds = 5
	}
	if backoffMaxSeconds <= 0 {
		backoffMaxSeconds = 900
	}
	if backoffPolicy == "" {
		backoffPolicy = "exp_full_jitter"
	}
	return &taskRepo{
		db:                 db,
		tz:                 tz,
		backoffPolicy:      backoffPolicy,
		backoffBaseSeconds: backoffBaseSeconds,
		backoffMaxSeconds:  backoffMaxSeconds,
		reconcile:          reconcileTracker{interval: defaultReconcileInterval},
	}
}

func (r *taskRepo) now() time.Time { return time.Now().In(r.tz) }

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

func (r *taskRepo) Enqueue(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, error) {
	task, _, err := r.EnqueueWithReady(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, visibleAt, tenantID)
	return task, err
}

func (r *taskRepo) EnqueueWithReady(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, visibleAt time.Time, tenantID string) (*domain.Task, bool, error) {
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
	id := uuid.NewString()
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
	if delayed {
		score := uint64(visibleAt.Unix())
		if err := b.Set(KeyDelayed(cmd, tenantID, score, id), nil, nil); err != nil {
			return nil, false, err
		}
	} else {
		seq := r.db.NextSeq()
		if err := b.Set(KeyPending(cmd, tenantID, priority, seq, id), nil, nil); err != nil {
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
	metrics.TaskCreatedTotal.WithLabelValues(string(cmd)).Inc()
	return task, ready, nil
}

// ---------- Get ----------

func (r *taskRepo) Get(ctx context.Context, taskID string) (*domain.Task, error) {
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

func (r *taskRepo) Claim(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, inspectLimit int, maxAttemptsDefault int, tenantID string) (*domain.Task, bool, error) {
	if inspectLimit <= 0 {
		inspectLimit = defaultInspectLimit
	}

	// Delayed→pending sweep on the hot path so Nack(delaySeconds=0) becomes
	// immediately claimable. Lease-expiry repair is throttled.
	for _, cmd := range commands {
		if _, err := r.MoveDueDelayed(ctx, cmd, inspectLimit); err != nil {
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
func (r *taskRepo) popFromAnyPriority(ctx context.Context, workerID string, cmd domain.Command, leaseSeconds int, tenantID string) (*domain.Task, bool, error) {
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

func (r *taskRepo) tryPopPriority(ctx context.Context, workerID string, cmd domain.Command, priority, leaseSeconds int, tenantID string) (*domain.Task, bool, error) {
	lockKey := string(cmd) + "\x00" + tenantID + "\x00" + strconv.Itoa(priority)
	muRaw, _ := r.queueLocks.LoadOrStore(lockKey, &sync.Mutex{})
	mu := muRaw.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	lower, upper := PrefixPendingPrio(cmd, tenantID, priority)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return nil, false, err
	}
	defer it.Close()
	if !it.First() {
		return nil, false, nil
	}

	pendingKey := append([]byte(nil), it.Key()...)
	id, ok := ParsePendingKey(pendingKey)
	if !ok || id == "" {
		return nil, false, fmt.Errorf("malformed pending key")
	}

	// Load task body now (single point lookup) so we can stamp the worker
	// and lease into the JSON inside the same batch.
	taskJSON, err := r.db.Get(KeyTask(id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Ghost: pending pointed at a task that's gone. Drop the pending
			// entry in its own delete; loop continues on next iteration.
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
	// Refresh TTL retention window.
	ttlScore := uint64(now.Add(taskRetention).Unix())
	if err := b.Set(KeyTTLIndex(ttlScore, id), nil, nil); err != nil {
		return nil, false, err
	}
	if err := r.db.CommitBatch(b); err != nil {
		return nil, false, fmt.Errorf("commit claim: %w", err)
	}
	return &t, true, nil
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

func (r *taskRepo) Heartbeat(ctx context.Context, taskID string, workerID string, extendSeconds int) error {
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

func (r *taskRepo) Abandon(ctx context.Context, taskID string, workerID string) error {
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
	return r.db.CommitBatch(b)
}

// ---------- Nack ----------

func (r *taskRepo) Nack(ctx context.Context, taskID string, workerID string, delaySeconds int, maxAttemptsDefault int, reason string) (int, bool, error) {
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
	return delaySeconds, false, nil
}

// ---------- MoveDueDelayed ----------

func (r *taskRepo) MoveDueDelayed(ctx context.Context, cmd domain.Command, limit int) (int, error) {
	return r.moveDueDelayedForTenant(ctx, cmd, limit, "")
}

func (r *taskRepo) moveDueDelayedForTenant(ctx context.Context, cmd domain.Command, limit int, tenantID string) (int, error) {
	if limit <= 0 {
		limit = defaultInspectLimit
	}
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
		moved++
	}
	if err := r.db.CommitBatch(b); err != nil {
		return 0, err
	}
	return moved, nil
}

// ---------- requeueExpired ----------

func (r *taskRepo) requeueExpired(ctx context.Context, cmd domain.Command, inspectLimit, maxAttemptsDefault int, tenantID string) (int, error) {
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

func (r *taskRepo) PendingLength(ctx context.Context, cmd domain.Command) (int64, error) {
	lower, upper := PrefixPendingAllPrios(cmd, "")
	return r.countPrefix(lower, upper)
}

func (r *taskRepo) QueueStats(ctx context.Context, cmd domain.Command, tenantID string) (*domain.QueueStats, error) {
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

func (r *taskRepo) AdminQueues(ctx context.Context) (map[string]any, error) {
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
		out[bucket] = out[bucket].(int64) + 1
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

func (r *taskRepo) countPrefix(lower, upper []byte) (int64, error) {
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
func (r *taskRepo) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
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
