package pebble

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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

	leaseInterval time.Duration // how often to scan lease/*
	ttlInterval   time.Duration // how often to scan ttl_index
	leaseBatch    int           // max leases to inspect per tick
	ttlBatch      int           // max ttl entries to clean per tick
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
	go r.loop(ctx, r.leaseInterval, r.sweepLeases, "lease")
	go r.loop(ctx, r.ttlInterval, r.sweepTTL, "ttl")
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

func (r *Reaper) loop(ctx context.Context, interval time.Duration, tick func(context.Context) (int, error), label string) {
	t := time.NewTicker(interval)
	defer t.Stop()
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
			n, err := tick(ctx)
			if err != nil {
				r.logger.Warn("reaper tick failed", "sweep", label, "err", err)
				continue
			}
			if n > 0 {
				r.logger.Debug("reaper tick", "sweep", label, "processed", n)
			}
		}
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
	now := time.Now().In(r.tz).Unix()
	ids := r.db.Leases.SnapshotExpired(now, r.leaseBatch)
	if len(ids) == 0 {
		return 0, nil
	}
	moved := 0
	for _, id := range ids {
		// Double-check the lease is still expired — a Heartbeat between
		// snapshot and now would have extended it.
		if e, ok := r.db.Leases.Get(id); ok && e.untilU > now {
			continue
		}
		taskJSON, err := r.db.Get(KeyTask(id))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				r.db.Leases.Delete(id)
				continue
			}
			return moved, err
		}
		var t domain.Task
		if err := sonic.Unmarshal(taskJSON, &t); err != nil {
			r.db.Leases.Delete(id)
			continue
		}
		if t.Status != domain.StatusInProgress {
			r.db.Leases.Delete(id)
			continue
		}
		metrics.LeaseExpiredTotal.WithLabelValues(string(t.Command)).Inc()
		delaySeconds := backoff.Compute(r.backoffPolicy, r.backoffBaseSeconds, r.backoffMaxSeconds, t.Attempts, nil)
		if err := r.requeueExpiredOne(ctx, &t, delaySeconds); err != nil {
			r.logger.Warn("reap requeue failed", "id", id, "err", err)
			continue
		}
		moved++
	}
	return moved, nil
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
	if err := b.Set(KeyDelayed(t.Command, t.TenantID, uint64(visibleAt.Unix()), t.ID), nil, nil); err != nil {
		return err
	}
	if err := b.Set(KeyTask(t.ID), updated, nil); err != nil {
		return err
	}
	if err := r.db.CommitBatch(b); err != nil {
		return err
	}
	r.db.Leases.Delete(t.ID)
	return nil
}

// sweepTTL drops aged-out task hashes (and their lease entries) up to
// ttlBatch per tick. CleanupExpired on the foreground path is bounded too;
// the reaper exists so an idle deployment still trims stale data.
func (r *Reaper) sweepTTL(ctx context.Context) (int, error) {
	before := time.Now().In(r.tz)
	lower, _ := PrefixTTL()
	upper := make([]byte, 0, len(pTTL)+8)
	upper = append(upper, pTTL...)
	upper = append(upper, be8(uint64(before.Unix())+1)...)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return 0, err
	}
	defer it.Close()

	type cand struct {
		ttlKey []byte
		id     string
	}
	bucket := make([]cand, 0, r.ttlBatch)
	for valid := it.First(); valid && len(bucket) < r.ttlBatch; valid = it.Next() {
		k := append([]byte(nil), it.Key()...)
		idx := strings.LastIndexByte(string(k), '/')
		if idx < 0 || idx+1 >= len(k) {
			continue
		}
		bucket = append(bucket, cand{ttlKey: k, id: string(k[idx+1:])})
	}
	if len(bucket) == 0 {
		return 0, nil
	}

	b := r.db.Batch()
	defer b.Close()
	for _, c := range bucket {
		if err := b.Delete(KeyTask(c.id), nil); err != nil {
			return 0, err
		}
		// Phase 6 / M2: KeyLease eliminated; in-memory drop below.
		if err := b.Delete(c.ttlKey, nil); err != nil {
			return 0, err
		}
	}
	if err := r.db.CommitBatch(b); err != nil {
		return 0, err
	}
	for _, c := range bucket {
		r.db.Leases.Delete(c.id)
	}
	return len(bucket), nil
}
