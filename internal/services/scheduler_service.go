package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type SchedulerService interface {
	CreateTask(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey, deduplicationKey string, runAt time.Time, delaySeconds int, tenantID string) (*domain.Task, error)
	ClaimTask(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, waitSeconds int, tenantID string) (*domain.Task, bool, error)
	// ClaimManyTasks pops up to max tasks in one round-trip when the
	// underlying repo supports batched claim (Pebble does via Phase 7's
	// ClaimMany). Falls back to looping ClaimTask for repos that don't.
	// Returns the claimed tasks in pop order; may return fewer than max
	// (or nil) if the queue emptied. waitSeconds=0 — caller controls
	// polling for the batch path.
	ClaimManyTasks(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, max int, tenantID string) ([]*domain.Task, error)
	Heartbeat(ctx context.Context, taskID, workerID string, extendSeconds int) error
	// ReportProgress stores the progress value the lease holder reports for
	// an in-progress task. It is validated by the caller.
	ReportProgress(ctx context.Context, taskID, workerID string, progress json.RawMessage) error
	Abandon(ctx context.Context, taskID, workerID string) error
	NackTask(ctx context.Context, taskID, workerID string, delaySeconds int, reason string) (int, bool, error)
	GetTask(ctx context.Context, id string) (*domain.Task, error)
	AdminQueues(ctx context.Context) (map[string]any, error)
	QueueStats(ctx context.Context, cmd domain.Command, tenantID string) (*domain.QueueStats, error)
	// ListTasks pages the tasks of one (cmd, tenant) queue state. limit 0
	// means DefaultTaskListLimit; a limit outside 1..MaxTaskListLimit fails
	// with domain.ErrInvalidListLimit.
	ListTasks(ctx context.Context, cmd domain.Command, tenantID string, state domain.QueueState, limit int, cursor string) (*domain.TaskPage, error)

	// Novo: limpeza administrativa por índice Z
	CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error)
}

// batchClaimer is the optional repo interface implemented by Pebble's
// TaskRepository (Phase 7). Redis-backed repos don't implement it and
// fall back to the loop in ClaimManyTasks.
type batchClaimer interface {
	ClaimMany(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, max int, inspectLimit int, maxAttemptsDefault int, tenantID string) ([]*domain.Task, error)
}

type schedulerService struct {
	repo                repository.TaskRepository
	notifier            NotifierService
	callback            ResultCallbackService
	tz                  *time.Location
	now                 func() time.Time
	defaultLease        int
	requeueInspectLimit int
	maxAttemptsDefault  int
	backoffMaxSeconds   int
}

func NewSchedulerService(repo repository.TaskRepository, notifier NotifierService, callback ResultCallbackService, tz *time.Location, now func() time.Time, defaultLease, inspectLimit, maxAttemptsDefault int, backoffPolicy string, backoffBaseSeconds int, backoffMaxSeconds int) SchedulerService {
	if maxAttemptsDefault <= 0 {
		maxAttemptsDefault = 5
	}
	if backoffMaxSeconds <= 0 {
		backoffMaxSeconds = 900
	}
	return &schedulerService{
		repo:                repo,
		notifier:            notifier,
		callback:            callback,
		tz:                  tz,
		now:                 now,
		defaultLease:        defaultLease,
		requeueInspectLimit: inspectLimit,
		maxAttemptsDefault:  maxAttemptsDefault,
		backoffMaxSeconds:   backoffMaxSeconds,
	}
}

// validateCreate rejects a create the scheduler must not enqueue: an empty
// command, a webhook that is not an absolute http(s) URL, or both an
// idempotency and a deduplication key. It marks the span the same way the
// inline checks it replaces did.
func validateCreate(span trace.Span, cmd domain.Command, webhook, idempotencyKey, deduplicationKey string) error {
	if strings.TrimSpace(string(cmd)) == "" {
		span.SetStatus(codes.Error, "invalid command")
		return errors.New("invalid command")
	}
	if webhook != "" {
		u, err := url.Parse(webhook)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			span.RecordError(err)
			span.SetStatus(codes.Error, "invalid webhook url")
			return errors.New("invalid webhook url")
		}
	}
	if idempotencyKey != "" && deduplicationKey != "" {
		span.SetStatus(codes.Error, domain.ErrDeduplicationWithIdempotency.Error())
		return domain.ErrDeduplicationWithIdempotency
	}
	return nil
}

// visibleAt is when a new task becomes claimable: runAt wins over
// delaySeconds, and the zero time means immediately.
func (s *schedulerService) visibleAt(runAt time.Time, delaySeconds int) time.Time {
	if !runAt.IsZero() {
		return runAt
	}
	if delaySeconds > 0 {
		return s.now().Add(time.Duration(delaySeconds) * time.Second)
	}
	return time.Time{}
}

// CreateTask validates and enqueues a task, or returns the task an
// idempotency or deduplication key resolves to.
func (s *schedulerService) CreateTask(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey, deduplicationKey string, runAt time.Time, delaySeconds int, tenantID string) (*domain.Task, error) {
	ctx, span := otel.Tracer("codeq/scheduler").Start(ctx, "codeq.task.create",
		trace.WithAttributes(
			attribute.String("codeq.command", string(cmd)),
			attribute.Int("codeq.priority", priority),
			attribute.Bool("codeq.has_webhook", strings.TrimSpace(webhook) != ""),
			attribute.Bool("codeq.has_idempotency_key", strings.TrimSpace(idempotencyKey) != ""),
			attribute.String("codeq.tenant_id", tenantID),
		),
	)
	defer span.End()
	if deduplicationKey != "" {
		// Set only when present: an attribute in the start options costs an
		// allocation on every create, including the ones without a key.
		span.SetAttributes(attribute.Bool("codeq.has_deduplication_key", true))
	}

	if err := validateCreate(span, cmd, webhook, idempotencyKey, deduplicationKey); err != nil {
		return nil, err
	}
	if maxAttempts <= 0 {
		maxAttempts = s.maxAttemptsDefault
	}
	visibleAt := s.visibleAt(runAt, delaySeconds)

	task, ready, err := s.repo.EnqueueWithReady(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, deduplicationKey, visibleAt, tenantID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.String("codeq.task_id", task.ID))
	if s.notifier != nil && ready {
		// `ready` is true only when this insert transitioned the priority queue from 0→1
		// (immediate tasks only). The signal comes from the LPush result inside the enqueue
		// pipeline, so we no longer pay a separate LLEN RTT on the create hot path.
		s.notifier.NotifyQueueReady(ctx, cmd)
	}
	return task, nil
}

// ClaimManyTasks dispatches to repo.ClaimMany when the backend supports
// it (Pebble), or falls back to looping ClaimTask. The fast path packs
// up to `max` claims into one Pebble batch with one commit. Single-RTT
// regardless of N, which is what makes Phase 6 Q2's worker batch
// actually faster instead of just structurally batched.
func (s *schedulerService) ClaimManyTasks(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, max int, tenantID string) ([]*domain.Task, error) {
	if workerID == "" {
		return nil, errors.New("workerId is required")
	}
	if max <= 0 {
		return nil, nil
	}
	if len(commands) == 0 {
		commands = []domain.Command{domain.CmdGenerateMaster, domain.CmdGenerateCreative}
	}
	if leaseSeconds <= 0 {
		leaseSeconds = s.defaultLease
	}
	if bc, ok := s.repo.(batchClaimer); ok {
		tasks, err := bc.ClaimMany(ctx, workerID, commands, leaseSeconds, max, s.requeueInspectLimit, s.maxAttemptsDefault, tenantID)
		for _, t := range tasks {
			metrics.TaskClaimedTotal.WithLabelValues(string(t.Command)).Inc()
		}
		return tasks, err
	}
	// Fallback: loop ClaimTask (no batch optimisation on this backend).
	out := make([]*domain.Task, 0, max)
	for range max {
		t, ok, err := s.repo.Claim(ctx, workerID, commands, leaseSeconds, s.requeueInspectLimit, s.maxAttemptsDefault, tenantID)
		if err != nil {
			return out, err
		}
		if !ok || t == nil {
			break
		}
		metrics.TaskClaimedTotal.WithLabelValues(string(t.Command)).Inc()
		out = append(out, t)
	}
	return out, nil
}

func (s *schedulerService) ClaimTask(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, waitSeconds int, tenantID string) (*domain.Task, bool, error) {
	if workerID == "" {
		return nil, false, errors.New("workerId is required")
	}
	if len(commands) == 0 {
		commands = []domain.Command{domain.CmdGenerateMaster, domain.CmdGenerateCreative}
	}
	if leaseSeconds <= 0 {
		leaseSeconds = s.defaultLease
	}
	if waitSeconds <= 0 {
		task, ok, err := s.repo.Claim(ctx, workerID, commands, leaseSeconds, s.requeueInspectLimit, s.maxAttemptsDefault, tenantID)
		if ok && task != nil {
			metrics.TaskClaimedTotal.WithLabelValues(string(task.Command)).Inc()
		}
		return task, ok, err
	}
	if waitSeconds > 30 {
		waitSeconds = 30
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		task, ok, err := s.repo.Claim(ctx, workerID, commands, leaseSeconds, s.requeueInspectLimit, s.maxAttemptsDefault, tenantID)
		if err != nil || ok {
			if ok && task != nil {
				metrics.TaskClaimedTotal.WithLabelValues(string(task.Command)).Inc()
			}
			return task, ok, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, false, nil
		}
		sleep := 250 * time.Millisecond
		if remaining < sleep {
			sleep = remaining
		}
		timer.Reset(sleep)
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *schedulerService) Heartbeat(ctx context.Context, taskID, workerID string, extendSeconds int) error {
	if extendSeconds <= 0 {
		extendSeconds = s.defaultLease
	}
	return s.repo.Heartbeat(ctx, taskID, workerID, extendSeconds)
}

// ReportProgress passes the validated progress value to the repository.
func (s *schedulerService) ReportProgress(ctx context.Context, taskID, workerID string, progress json.RawMessage) error {
	return s.repo.Progress(ctx, taskID, workerID, progress)
}

func (s *schedulerService) Abandon(ctx context.Context, taskID, workerID string) error {
	return s.repo.Abandon(ctx, taskID, workerID)
}

func (s *schedulerService) NackTask(ctx context.Context, taskID, workerID string, delaySeconds int, reason string) (int, bool, error) {
	if workerID == "" {
		return 0, false, errors.New("workerId is required")
	}
	t, err := s.repo.Get(ctx, taskID)
	if err != nil {
		return 0, false, err
	}
	if t.WorkerID != workerID {
		return 0, false, errors.New("not-owner")
	}
	if t.Status != domain.StatusInProgress {
		return 0, false, errors.New("not-in-progress")
	}
	if delaySeconds < 0 {
		delaySeconds = 0
	}
	if delaySeconds > s.backoffMaxSeconds {
		delaySeconds = s.backoffMaxSeconds
	}
	attempts, terminal, err := s.repo.Nack(ctx, taskID, workerID, delaySeconds, s.maxAttemptsDefault, reason)
	if err != nil {
		return attempts, terminal, err
	}
	if terminal && s.callback != nil {
		effectiveReason := reason
		if effectiveReason == "" {
			effectiveReason = "MAX_ATTEMPTS"
		}
		rec := domain.ResultRecord{
			TaskID:      taskID,
			Status:      domain.StatusFailed,
			Error:       effectiveReason,
			CompletedAt: s.now().In(s.tz),
		}
		s.callback.Send(context.WithoutCancel(ctx), *t, rec)
	}
	return attempts, terminal, nil
}

func (s *schedulerService) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	return s.repo.Get(ctx, id)
}

func (s *schedulerService) AdminQueues(ctx context.Context) (map[string]any, error) {
	return s.repo.AdminQueues(ctx)
}

func (s *schedulerService) QueueStats(ctx context.Context, cmd domain.Command, tenantID string) (*domain.QueueStats, error) {
	return s.repo.QueueStats(ctx, cmd, tenantID)
}

const (
	// DefaultTaskListLimit is the page size of a task listing that sets none.
	DefaultTaskListLimit = 100
	// MaxTaskListLimit bounds a page: every task carries its payload.
	MaxTaskListLimit = 500
)

// ListTasks validates the listing request and pages the repository.
func (s *schedulerService) ListTasks(ctx context.Context, cmd domain.Command, tenantID string, state domain.QueueState, limit int, cursor string) (*domain.TaskPage, error) {
	if strings.TrimSpace(string(cmd)) == "" {
		return nil, errors.New("invalid command")
	}
	if _, err := domain.ParseQueueState(string(state)); err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = DefaultTaskListLimit
	}
	if limit < 1 || limit > MaxTaskListLimit {
		return nil, domain.ErrInvalidListLimit
	}
	return s.repo.ListTasks(ctx, cmd, tenantID, state, limit, cursor)
}

func (s *schedulerService) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	if before.IsZero() {
		before = s.now()
	}
	if limit <= 0 {
		limit = 1000
	}
	return s.repo.CleanupExpired(ctx, limit, before)
}
