package services

import (
	"context"
	"log/slog"
	"time"

	"github.com/osvaldoandrade/codeq/internal/repository"
)

type SubscriptionCleanupService interface {
	Start(ctx context.Context)
}

type subscriptionCleanupService struct {
	repo     repository.SubscriptionRepository
	logger   *slog.Logger
	interval time.Duration
}

func NewSubscriptionCleanupService(repo repository.SubscriptionRepository, logger *slog.Logger, intervalSeconds int) SubscriptionCleanupService {
	if intervalSeconds <= 0 {
		intervalSeconds = 60
	}
	return &subscriptionCleanupService{
		repo:     repo,
		logger:   logger,
		interval: time.Duration(intervalSeconds) * time.Second,
	}
}

func (s *subscriptionCleanupService) Start(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed, err := s.repo.CleanupExpired(ctx, 1000, time.Now())
			if err != nil {
				s.logger.Warn("subscription cleanup failed", "err", err)
				continue
			}
			if removed > 0 {
				s.logger.Info("subscription cleanup removed", "count", removed)
			}
		}
	}
}
