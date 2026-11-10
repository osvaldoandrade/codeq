package pebble

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/bytedance/sonic"

	"github.com/osvaldoandrade/codeq/internal/backoff"
	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// Reaper sweeps expired leases and aged-out TTL entries on a timer.
// Redis gives us this for free via key TTLs + the claim-path repair
// sweep; with Pebble we run a goroutine to enforce the same semantics.
//
// The reaper holds no state besides timers; it queries the DB directly
// and reuses the same Nack/Cleanup helpers as the foreground path so
// behavior stays uniform.
type Reaper struct {
	db                 *DB
	tz                 *time.Location
	logger             *slog.Logger
	backoffPolicy      string
	backoffBaseSeconds int
	backoffMaxSeconds  int
	maxAttemptsDefault int
	dlqCallback        func(ctx context.Context, t domain.Task, rec domain.ResultRecord)
	// leaderGate, when non-nil, is invoked at the start of every tick.
	// Returning false skips the tick. Used in raft mode so only the
	// leader sweeps — followers would either write through raft.Apply
	// (and fail with ErrNotLeader) or duplicate work the leader is
	// already doing.
	leaderGate func() bool
	// leaderReady, when non-nil, runs after leaderGate passes and must
	// succeed before the tick sweeps (see ReaperOptions.LeaderReady).
	leaderReady func(context.Context) error
	onDelayed   func(cmd domain.Command, tenantID string)

	leaseInterval time.Duration // how often to scan lease/*
	ttlInterval   time.Duration // how often to scan ttl_index
	leaseBatch    int           // max leases to inspect per tick
	ttlBatch      int           // max ttl entries to clean per tick

	// now is the clock of the stall watch; nil means time.Now.
	now func() time.Time
}

// ReaperOptions configures the cadence and batch sizes; zero values pick
// sane defaults appropriate for the bench (aggressive enough to be
// observable, cheap enough to stay out of the way).
type ReaperOptions struct {
	LeaseInterval      time.Duration
	TTLInterval        time.Duration
	LeaseBatch         int
	TTLBatch           int
	BackoffPolicy      string
	BackoffBaseSeconds int
	BackoffMaxSeconds  int
	MaxAttemptsDefault int
	// DLQCallback fires when a task is moved to DLQ due to lease expiry
	// (after max attempts). Invoked once per task, post-commit, with a
	// synthetic ResultRecord describing the failure. Optional.
	DLQCallback func(ctx context.Context, t domain.Task, rec domain.ResultRecord)
	// LeaderGate, when set, is consulted at the start of every reaper
	// tick. When it returns false the tick is a no-op. Wired by
	// pkg/app/application_pebble.go to raft.DB.IsLeader when raft is
	// enabled; nil otherwise (single-node deployments run every tick).
	LeaderGate func() bool
	// LeaderReady, when set, runs after LeaderGate passes; an error skips
	// the tick. Wired to TaskRepository.EnsureLeaderDispatch so a newly
	// elected leader sweeps only once a Raft barrier has applied every
	// entry committed before its term and the dispatch index, including
	// the lease table, has been rebuilt from that state (ADR 0015). A
	// voter that took over right after its own restart otherwise read the
	// snapshot it had just restored, older than the log it was about to
	// apply, and wrote requeues and retirements computed from that stale
	// state. It also completes a rebuild the election-time attempt left
	// unfinished on a leader that receives no request.
	LeaderReady func(context.Context) error
	// OnDelayed fires after a lease-expired task is written to the
	// delayed bucket. The repository uses it to bump the process-local
	// delayed counter; without that bump Claim skips the delayed scan
	// and the task never becomes pending again.
	OnDelayed func(cmd domain.Command, tenantID string)
}

func NewReaper(db *DB, tz *time.Location, logger *slog.Logger, opts ReaperOptions) *Reaper {
	if opts.LeaseInterval <= 0 {
		opts.LeaseInterval = 2 * time.Second
	}
	if opts.TTLInterval <= 0 {
		opts.TTLInterval = 30 * time.Second
	}
	if opts.LeaseBatch <= 0 {
		opts.LeaseBatch = 256
	}
	if opts.TTLBatch <= 0 {
		opts.TTLBatch = 512
	}
	if opts.BackoffPolicy == "" {
		opts.BackoffPolicy = "exp_full_jitter"
	}
	if opts.BackoffBaseSeconds <= 0 {
		opts.BackoffBaseSeconds = 5
	}
	if opts.BackoffMaxSeconds <= 0 {
		opts.BackoffMaxSeconds = 900
	}
	if opts.MaxAttemptsDefault <= 0 {
		opts.MaxAttemptsDefault = 5
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reaper{
		db:                 db,
		tz:                 tz,
		logger:             logger.With("component", "pebble.reaper"),
		backoffPolicy:      opts.BackoffPolicy,
		backoffBaseSeconds: opts.BackoffBaseSeconds,
		backoffMaxSeconds:  opts.BackoffMaxSeconds,
		maxAttemptsDefault: opts.MaxAttemptsDefault,
		dlqCallback:        opts.DLQCallback,
		leaderGate:         opts.LeaderGate,
		leaderReady:        opts.LeaderReady,
		onDelayed:          opts.OnDelayed,
		leaseInterval:      opts.LeaseInterval,
		ttlInterval:        opts.TTLInterval,
		leaseBatch:         opts.LeaseBatch,
		ttlBatch:           opts.TTLBatch,
	}
}

// Start runs the reaper loops until ctx is cancelled. Returns nil when
// shutdown is graceful; logs and continues on individual tick errors so
// transient failures don't take the reaper offline.
func (r *Reaper) Start(ctx context.Context) {
	go r.loop(ctx, r.leaseInterval, r.leaseSweep, sweepLease)
	go r.loop(ctx, r.ttlInterval, r.ttlSweep, sweepTTLLabel)
}

const (
	sweepLease    = "lease"
	sweepTTLLabel = "ttl"

	outcomeIdle     = "idle"
	outcomeProgress = "progress"
	outcomeStalled  = "stalled"
	outcomeError    = "error"
	outcomeDeferred = "deferred"

	// leaderReadyTimeout bounds the barrier and rebuild one tick waits for.
	leaderReadyTimeout = 2 * time.Second

	// stallWarnAfter is how long a sweep may keep finding due work it
	// cannot finish before the reaper warns, and stallWarnEvery how often
	// it repeats the warning while the stall lasts.
	stallWarnAfter = time.Minute
	stallWarnEvery = 5 * time.Minute
)

// sweepResult is what one reaper sweep did: the due entries it inspected and
// how many of them it finished (requeued, retired, or dropped from the
// in-memory or on-disk index). Due entries that stay are the work the next
// sweep reads again.
type sweepResult struct {
	due, done int
}

// outcome classifies a successful sweep.
func (s sweepResult) outcome() string {
	switch {
	case s.done > 0:
		return outcomeProgress
	case s.due == 0:
		return outcomeIdle
	default:
		return outcomeStalled
	}
}

// stallWatch rate-limits the warning of one sweep that keeps re-reading due
// work without finishing any of it. Only the sweep's own goroutine uses it.
type stallWatch struct {
	since    time.Time
	lastWarn time.Time
}

// observe records one sweep and reports whether to warn now.
func (w *stallWatch) observe(now time.Time, outcome string) bool {
	if outcome != outcomeStalled {
		w.since = time.Time{}
		return false
	}
	if w.since.IsZero() {
		w.since = now
	}
	if now.Sub(w.since) < stallWarnAfter || (!w.lastWarn.IsZero() && now.Sub(w.lastWarn) < stallWarnEvery) {
		return false
	}
	w.lastWarn = now
	return true
}

func (r *Reaper) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// StartReapersForShards is the Phase 8 helper: spawns one Reaper per
// shard, each sweeping its own DB. Sharing one reaper across shards
// would serialise their sweeps and undo the parallelism, so we keep
// them independent.
func StartReapersForShards(ctx context.Context, dbs []*DB, tz *time.Location, logger *slog.Logger, opts ReaperOptions) {
	for _, db := range dbs {
		NewReaper(db, tz, logger, opts).Start(ctx)
	}
}

func (r *Reaper) loop(ctx context.Context, interval time.Duration, tick func(context.Context) (sweepResult, error), label string) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var stall stallWatch
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if r.leaderGate != nil && !r.leaderGate() {
				// Follower in raft mode: leader's reaper handles all
				// sweeps. Failover catches up on the next tick after
				// election.
				continue
			}
			r.tickWhenReady(ctx, tick, label, &stall)
		}
	}
}

// tickWhenReady runs the sweep once this leader has applied its log and
// rebuilt its dispatch index; until then the tick is deferred, never run on
// a state older than the committed log.
func (r *Reaper) tickWhenReady(ctx context.Context, tick func(context.Context) (sweepResult, error), label string, stall *stallWatch) {
	if r.leaderReady != nil {
		readyCtx, cancel := context.WithTimeout(ctx, leaderReadyTimeout)
		err := r.leaderReady(readyCtx)
		cancel()
		if err != nil {
			metrics.ReaperTicks.WithLabelValues(label, outcomeDeferred).Inc()
			r.logger.Debug("reaper tick deferred until the leader has applied its log", "sweep", label, "err", err)
			return
		}
	}
	r.runTick(ctx, tick, label, stall)
}

// runTick runs one sweep and accounts for its outcome. A sweep that keeps
// finding due work and finishing none of it is a leader burning CPU without
// progress; it is counted on every tick and warned about, rate limited, with
// the amount of work it re-reads.
func (r *Reaper) runTick(ctx context.Context, tick func(context.Context) (sweepResult, error), label string, stall *stallWatch) {
	res, err := tick(ctx)
	if err != nil {
		metrics.ReaperTicks.WithLabelValues(label, outcomeError).Inc()
		r.logger.Warn("reaper tick failed", "sweep", label, "err", err)
		return
	}
	outcome := res.outcome()
	metrics.ReaperTicks.WithLabelValues(label, outcome).Inc()
	if outcome == outcomeProgress {
		r.logger.Debug("reaper tick", "sweep", label, "processed", res.done)
	}
	if stall.observe(r.clock(), outcome) {
		r.logger.Warn("reaper sweep is making no progress: due entries stay and are re-read every tick",
			"event", "codeq_reaper_stalled", "sweep", label, "due", res.due, "stalledFor", r.clock().Sub(stall.since).Round(time.Second).String())
	}
}

// sweepLeases snapshots up to leaseBatch entries from the in-memory
// lease table (Phase 6 / M2) whose until_unix is in the past, then
// requeues each via the same path as the foreground requeueExpired so
// retry policy / DLQ behavior stays consistent regardless of which
// path notices the expiry first. Pre-M2 this scanned KeyLease entries
// on disk; the in-memory swap removed a per-tick Pebble iter from the
// reaper's hot path AND eliminated 1 KeyLease write per Claim.
func (r *Reaper) sweepLeases(ctx context.Context) (int, error) {
	res, err := r.leaseSweepDetailed(ctx)
	return res.done - res.cleared, err
}

// leaseSweepResult extends sweepResult with the stale in-memory entries a
// lease sweep dropped (counted as done, not as moved).
type leaseSweepResult struct {
	sweepResult
	cleared int
}

func (r *Reaper) leaseSweep(ctx context.Context) (sweepResult, error) {
	res, err := r.leaseSweepDetailed(ctx)
	return res.sweepResult, err
}

func (r *Reaper) leaseSweepDetailed(ctx context.Context) (leaseSweepResult, error) {
	now := time.Now().In(r.tz).Unix()
	ids := r.db.Leases.SnapshotExpired(now, r.leaseBatch)
	if len(ids) == 0 {
		return leaseSweepResult{}, nil
	}
	moved, cleared := 0, 0
	result := func() leaseSweepResult {
		return leaseSweepResult{sweepResult: sweepResult{due: len(ids), done: moved + cleared}, cleared: cleared}
	}
	// Expired absolute rows retire together in one replicated batch at the
	// end of the tick instead of one commit per row.
	var expired []*domain.Task
	for _, id := range ids {
		t, action, err := r.inspectExpiredLease(id, now)
		if err != nil {
			err = joinErrors(err, r.retireExpired(expired, &moved))
			return result(), err
		}
		switch action {
		case leaseCleared:
			cleared++
		case leaseRetire:
			expired = append(expired, t)
		case leaseRequeue:
			if r.requeueExpiredLease(ctx, id, t) {
				moved++
			}
		}
	}
	err := r.retireExpired(expired, &moved)
	return result(), err
}

// leaseAction is what a lease sweep does with one expired lease entry.
type leaseAction int

const (
	// leaseCleared: the entry is stale (or was extended) and leaves the
	// table without a write.
	leaseCleared leaseAction = iota
	// leaseRetire: the row's absolute deadline passed; it retires with the
	// tick's batch.
	leaseRetire
	// leaseRequeue: the in-progress row goes back to delayed (or DLQ).
	leaseRequeue
)

// inspectExpiredLease reads the row behind one expired lease entry and
// decides its fate, dropping a stale entry from the table on the spot.
func (r *Reaper) inspectExpiredLease(id string, now int64) (*domain.Task, leaseAction, error) {
	// Double-check the lease is still expired — a Heartbeat between
	// snapshot and now would have extended it.
	if e, ok := r.db.Leases.Get(id); ok && e.untilU > now {
		return nil, leaseCleared, nil
	}
	taskJSON, err := r.db.Get(KeyTask(id))
	if errors.Is(err, ErrNotFound) {
		r.db.Leases.Delete(id)
		return nil, leaseCleared, nil
	}
	if err != nil {
		return nil, leaseCleared, err
	}
	t, decoded := decodeTask(taskJSON)
	if !decoded {
		// An undecodable row has no lease to enforce.
		r.db.Leases.Delete(id)
		return nil, leaseCleared, nil
	}
	if err := r.db.checkTaskExpiry(t); err != nil {
		if errors.Is(err, domain.ErrTaskExpired) {
			return t, leaseRetire, nil
		}
		return nil, leaseCleared, err
	}
	if t.Status != domain.StatusInProgress {
		r.db.Leases.Delete(id)
		return nil, leaseCleared, nil
	}
	return t, leaseRequeue, nil
}

// decodeTask decodes a task body and reports whether it decoded.
func decodeTask(body []byte) (*domain.Task, bool) {
	var t domain.Task
	return &t, sonic.Unmarshal(body, &t) == nil
}

// requeueExpiredLease requeues one in-progress row whose lease expired and
// reports whether it moved. A failure is logged and leaves the entry for the
// next sweep.
func (r *Reaper) requeueExpiredLease(ctx context.Context, id string, t *domain.Task) bool {
	metrics.LeaseExpiredTotal.WithLabelValues(string(t.Command)).Inc()
	delaySeconds := backoff.Compute(r.backoffPolicy, r.backoffBaseSeconds, r.backoffMaxSeconds, t.Attempts, nil)
	if err := r.requeueExpiredOne(ctx, t, delaySeconds); err != nil {
		r.logger.Warn("reap requeue failed", "id", id, "err", err)
		return false
	}
	return true
}

// retireExpired retires the expired rows a lease sweep met in one batch and
// counts them as moved.
func (r *Reaper) retireExpired(expired []*domain.Task, moved *int) error {
	retired, err := r.db.purgeAbsoluteRows(expired)
	for _, ok := range retired {
		if ok {
			*moved++
		}
	}
	return err
}

// requeueExpiredOne moves a single in-progress task back into delayed with
// backoff (or DLQ if it has exhausted its attempts). Mirrors taskRepo.Nack
// but skips ownership checks since we're the reaper.
func (r *Reaper) requeueExpiredOne(ctx context.Context, t *domain.Task, delaySeconds int) error {
	now := time.Now().In(r.tz)
	t.Attempts++
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = r.maxAttemptsDefault
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = 1
	}

	b := r.db.Batch()
	defer b.Close()
	if err := b.Delete(KeyInprog(t.Command, t.TenantID, t.ID), nil); err != nil {
		return err
	}
	// Phase 6 / M2: KeyLease eliminated; in-memory drop happens after
	// CommitBatch below so a failed commit doesn't lose the lease.

	if t.Attempts >= t.MaxAttempts {
		t.Status = domain.StatusFailed
		t.LastKnownLocation = domain.LocationDLQ
		t.WorkerID = ""
		t.LeaseUntil = ""
		t.Error = "LEASE_EXPIRED"
		t.UpdatedAt = now
		updated, _ := sonic.Marshal(t)
		if err := b.Set(KeyDLQ(t.Command, t.TenantID, t.ID), nil, nil); err != nil {
			return err
		}
		if err := b.Set(KeyTask(t.ID), updated, nil); err != nil {
			return err
		}
		if err := r.db.CommitBatch(b); err != nil {
			return err
		}
		r.db.Leases.Delete(t.ID)
		metrics.TaskCompletedTotal.WithLabelValues(string(t.Command), string(t.Status)).Inc()
		if r.dlqCallback != nil {
			rec := domain.ResultRecord{
				TaskID:      t.ID,
				Status:      domain.StatusFailed,
				Error:       t.Error,
				CompletedAt: now,
			}
			r.dlqCallback(context.WithoutCancel(ctx), *t, rec)
		}
		return nil
	}

	visibleAt := now.Add(time.Duration(delaySeconds) * time.Second).UTC()
	t.Status = domain.StatusPending
	t.LastKnownLocation = domain.LocationDelayed
	t.WorkerID = ""
	t.LeaseUntil = ""
	t.UpdatedAt = now
	updated, _ := sonic.Marshal(t)
	if err := b.Set(KeyDelayed(t.Command, t.TenantID, unixSeconds(visibleAt), t.ID), nil, nil); err != nil {
		return err
	}
	if err := b.Set(KeyTask(t.ID), updated, nil); err != nil {
		return err
	}
	if err := r.db.CommitBatch(b); err != nil {
		return err
	}
	r.db.Leases.Delete(t.ID)
	if r.onDelayed != nil {
		r.onDelayed(t.Command, t.TenantID)
	}
	return nil
}

// sweepTTL drops aged-out task hashes (and their lease entries) up to
// ttlBatch per tick. CleanupExpired on the foreground path is bounded too;
// the reaper exists so an idle deployment still trims stale data.
func (r *Reaper) sweepTTL(ctx context.Context) (int, error) {
	res, err := r.ttlSweepDetailed(ctx)
	return res.removed, err
}

// ttlSweepResult extends sweepResult with the rows the sweep removed.
type ttlSweepResult struct {
	sweepResult
	removed int
}

func (r *Reaper) ttlSweep(ctx context.Context) (sweepResult, error) {
	res, err := r.ttlSweepDetailed(ctx)
	return res.sweepResult, err
}

// ttlSweepDetailed counts the due TTL index entries as the sweep's due work:
// an entry the sweep cannot drop stays at the head of the index and is read
// again by every later sweep. The rotating absolute sweep that runs first is
// not counted; most identities it visits legitimately need nothing.
func (r *Reaper) ttlSweepDetailed(ctx context.Context) (ttlSweepResult, error) {
	absoluteRemoved, err := r.db.sweepAbsolute(ctx, r.ttlBatch)
	if err != nil {
		return ttlSweepResult{}, err
	}
	before := time.Now().In(r.tz)
	lower, _ := PrefixTTL()
	upper := make([]byte, 0, len(pTTL)+8)
	upper = append(upper, pTTL...)
	upper = append(upper, be8(unixSeconds(before)+1)...)
	hits, err := collectTTLHits(r.db, lower, upper, r.ttlBatch)
	if err != nil || len(hits) == 0 {
		return ttlSweepResult{sweepResult: sweepResult{done: absoluteRemoved}, removed: absoluteRemoved}, err
	}
	n, err := dropExpiredBodies(r.db, hits)
	if err != nil {
		return ttlSweepResult{removed: absoluteRemoved + n}, err
	}
	left, err := remainingTTLHits(r.db, hits)
	return ttlSweepResult{sweepResult: sweepResult{due: len(hits), done: absoluteRemoved + len(hits) - left}, removed: absoluteRemoved + n}, err
}

// remainingTTLHits counts the due index entries a sweep left in place; the
// next sweep reads them again. At most ttlBatch point reads per tick.
func remainingTTLHits(db *DB, hits []ttlHit) (int, error) {
	left := 0
	for _, hit := range hits {
		exists, err := db.Has(hit.key)
		if err != nil {
			return left, err
		}
		if exists {
			left++
		}
	}
	return left, nil
}
