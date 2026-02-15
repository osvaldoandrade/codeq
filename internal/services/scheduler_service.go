package services

import (
	"context"
	"errors"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/internal/backoff"
	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

type SchedulerService interface {
	CreateTask(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, runAt time.Time, delaySeconds int, tenantID string) (*domain.Task, error)
	ClaimTask(ctx context.Context, workerID string, commands []domain.Command, leaseSeconds int, waitSeconds int, tenantID string) (*domain.Task, bool, error)
	Heartbeat(ctx context.Context, taskID, workerID string, extendSeconds int) error
	Abandon(ctx context.Context, taskID, workerID string) error
	NackTask(ctx context.Context, taskID, workerID string, delaySeconds int, reason string) (int, bool, error)
	GetTask(ctx context.Context, id string) (*domain.Task, error)
	AdminQueues(ctx context.Context) (map[string]any, error)
	QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error)

	// Novo: limpeza administrativa por índice Z
	CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error)
}

type schedulerService struct {
	repo                repository.TaskRepository
	notifier            NotifierService
	tz                  *time.Location
	now                 func() time.Time
	defaultLease        int
	requeueInspectLimit int
	maxAttemptsDefault  int
	backoffPolicy       string
	backoffBaseSeconds  int
	backoffMaxSeconds   int
	rng                 *rand.Rand
}

func NewSchedulerService(repo repository.TaskRepository, notifier NotifierService, tz *time.Location, now func() time.Time, defaultLease, inspectLimit, maxAttemptsDefault int, backoffPolicy string, backoffBaseSeconds int, backoffMaxSeconds int) SchedulerService {
	if maxAttemptsDefault <= 0 {
		maxAttemptsDefault = 5
	}
	if backoffBaseSeconds <= 0 {
		backoffBaseSeconds = 5
	}
	if backoffMaxSeconds <= 0 {
		backoffMaxSeconds = 900
	}
	if backoffPolicy == "" {
		backoffPolicy = "exp_full_jitter"
	}
	return &schedulerService{
		repo:                repo,
		notifier:            notifier,
		tz:                  tz,
		now:                 now,
		defaultLease:        defaultLease,
		requeueInspectLimit: inspectLimit,
		maxAttemptsDefault:  maxAttemptsDefault,
		backoffPolicy:       backoffPolicy,
		backoffBaseSeconds:  backoffBaseSeconds,
		backoffMaxSeconds:   backoffMaxSeconds,
		rng:                 rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *schedulerService) CreateTask(ctx context.Context, cmd domain.Command, payload string, priority int, webhook string, maxAttempts int, idempotencyKey string, runAt time.Time, delaySeconds int, tenantID string) (*domain.Task, error) {
	if strings.TrimSpace(string(cmd)) == "" {
		return nil, errors.New("invalid command")
	}
	if webhook != "" {
		u, err := url.Parse(webhook)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("invalid webhook url")
		}
	}
	if maxAttempts <= 0 {
		maxAttempts = s.maxAttemptsDefault
	}

	visibleAt := time.Time{}
	if !runAt.IsZero() {
		visibleAt = runAt
	} else if delaySeconds > 0 {
		visibleAt = s.now().Add(time.Duration(delaySeconds) * time.Second)
	}

	task, err := s.repo.Enqueue(ctx, cmd, payload, priority, webhook, maxAttempts, idempotencyKey, visibleAt, tenantID)
	if err != nil {
		return nil, err
	}
	if s.notifier != nil {
		// Only notify for immediate tasks (scheduled tasks are placed into delayed ZSET).
		if visibleAt.IsZero() || !visibleAt.After(s.now()) {
			if depth, err := s.repo.PendingLength(ctx, cmd); err == nil && depth == 1 {
				s.notifier.NotifyQueueReady(ctx, cmd)
			}
		}
	}
	return task, nil
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
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

func (s *schedulerService) Heartbeat(ctx context.Context, taskID, workerID string, extendSeconds int) error {
	if extendSeconds <= 0 {
		extendSeconds = s.defaultLease
	}
	return s.repo.Heartbeat(ctx, taskID, workerID, extendSeconds)
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
	if delaySeconds <= 0 {
		delaySeconds = s.computeBackoff(t.Attempts)
	}
	if delaySeconds > s.backoffMaxSeconds {
		delaySeconds = s.backoffMaxSeconds
	}
	return s.repo.Nack(ctx, taskID, workerID, delaySeconds, s.maxAttemptsDefault, reason)
}

func (s *schedulerService) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	return s.repo.Get(ctx, id)
}

func (s *schedulerService) AdminQueues(ctx context.Context) (map[string]any, error) {
	return s.repo.AdminQueues(ctx)
}

func (s *schedulerService) QueueStats(ctx context.Context, cmd domain.Command) (*domain.QueueStats, error) {
	return s.repo.QueueStats(ctx, cmd)
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

func (s *schedulerService) computeBackoff(attempts int) int {
	return backoff.Compute(s.backoffPolicy, s.backoffBaseSeconds, s.backoffMaxSeconds, attempts, s.rng)
}
