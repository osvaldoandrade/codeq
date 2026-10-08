package schedules

import (
	"context"
	"log/slog"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	defaultTickInterval = time.Second
	fireTimeout         = 10 * time.Second

	outcomeEnqueued   = "enqueued"
	outcomeFailed     = "failed"
	outcomeSuperseded = "superseded"
)

// TaskCreator enqueues the task of a slot. The scheduler service satisfies
// it; the slot's idempotency key makes a repeated call return the task the
// first call enqueued.
type TaskCreator interface {
	CreateTask(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey, deduplicationKey string, runAt time.Time, delaySeconds int, tenantID string) (*domain.Task, error)
}

// RunnerOptions tunes the firing loop. Zero values select the defaults.
type RunnerOptions struct {
	// Interval between due scans; default one second.
	Interval time.Duration
	// LeaderGate reports whether this node may fire. Nil means always (a
	// node without Raft is its own leader).
	LeaderGate func() bool
	Now        func() time.Time
	Logger     *slog.Logger
}

// Runner fires due schedules. Each slot enqueues exactly one task even
// across crashes and leader changes: the task is created under the slot's
// idempotency key before the slot is recorded, so a slot fired again (by
// this node after a failed Advance, or by a new leader that never saw the
// Advance) resolves to the task already enqueued.
type Runner struct {
	store    Store
	tasks    TaskCreator
	parse    schedule.Parser
	interval time.Duration
	leader   func() bool
	now      func() time.Time
	logger   *slog.Logger
}

// NewRunner builds the firing loop; Start runs it.
func NewRunner(store Store, tasks TaskCreator, parse schedule.Parser, opts RunnerOptions) *Runner {
	r := &Runner{store: store, tasks: tasks, parse: parse, interval: opts.Interval, leader: opts.LeaderGate, now: opts.Now, logger: opts.Logger}
	if r.interval <= 0 {
		r.interval = defaultTickInterval
	}
	if r.leader == nil {
		r.leader = func() bool { return true }
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	return r
}

// Start runs the loop until ctx is done.
func (r *Runner) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.tick(ctx)
			}
		}
	}()
}

// tick fires every due schedule once. Errors are logged and retried on the
// next tick: a slot is recorded only after its task exists.
func (r *Runner) tick(ctx context.Context) {
	if !r.leader() {
		return
	}
	due, err := r.store.Due(ctx, r.now())
	if err != nil {
		r.logger.Warn("schedule due scan failed", "err", err)
		return
	}
	for _, s := range due {
		if ctx.Err() != nil || !r.leader() {
			return
		}
		r.fire(ctx, s)
	}
}

// fire enqueues the task of the schedule's due slot and records the slot
// with the next one. Slots missed while no leader ran are not replayed one
// by one: the earliest missed slot fires once and the schedule resumes from
// the first slot after now.
func (r *Runner) fire(ctx context.Context, s schedule.Schedule) {
	ctx, cancel := context.WithTimeout(ctx, fireTimeout)
	defer cancel()
	slot := s.NextRunAt
	task, err := r.tasks.CreateTask(ctx, domain.Command(s.Spec.Command), s.Spec.TaskPayload(), s.Spec.Priority,
		s.Spec.Webhook, s.Spec.MaxAttempts, schedule.SlotKey(s.ScheduleID, slot), "", time.Time{}, 0, s.TenantID)
	if err != nil {
		metrics.ScheduleFiresTotal.WithLabelValues(outcomeFailed).Inc()
		r.logger.Warn("schedule fire failed; retrying next tick", "schedule", s.ScheduleID, "slot", slot, "err", err)
		return
	}
	expr, err := r.parse(s.Spec.Cron, s.Spec.Timezone)
	if err != nil {
		r.logger.Error("stored schedule no longer parses", "schedule", s.ScheduleID, "err", err)
		return
	}
	now := r.now()
	next := expr.Next(later(now, slot)).UTC()
	applied, err := r.store.Advance(ctx, s.ScheduleID, s.Version, slot, task.ID, next)
	if err != nil {
		r.logger.Warn("schedule advance failed; the slot replays to the same task", "schedule", s.ScheduleID, "slot", slot, "err", err)
		return
	}
	if !applied {
		metrics.ScheduleFiresTotal.WithLabelValues(outcomeSuperseded).Inc()
		return
	}
	metrics.ScheduleFiresTotal.WithLabelValues(outcomeEnqueued).Inc()
	metrics.ScheduleFireDelaySeconds.Observe(now.Sub(slot).Seconds())
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
