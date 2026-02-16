package services

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestNewNotifierServiceDefaults(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	repo := repository.NewSubscriptionRepository(rdb, time.UTC)

	tests := []struct {
		name      string
		minNotify int
	}{
		{"zero minNotify", 0},
		{"negative minNotify", -1},
		{"positive minNotify", 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewNotifierService(repo, slog.Default(), "secret", tt.minNotify, nil, ratelimit.Bucket{}, nil)
			if svc == nil {
				t.Fatal("Expected service to be non-nil")
			}
		})
	}
}

func TestNotifierServiceNotifyQueueReadyNoSubscriptions(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	repo := repository.NewSubscriptionRepository(rdb, time.UTC)
	svc := NewNotifierService(repo, slog.Default(), "secret", 5, nil, ratelimit.Bucket{}, nil)

	// Should not panic with no subscriptions
	ctx := context.Background()
	svc.NotifyQueueReady(ctx, domain.CmdGenerateMaster)
}

func TestNotifierServiceNotifyQueueReadyWithSubscription(t *testing.T) {
	mr, _ := miniredis.Run()
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	repo := repository.NewSubscriptionRepository(rdb, time.UTC)
	svc := NewNotifierService(repo, slog.Default(), "secret", 5, nil, ratelimit.Bucket{}, nil)

	// Create a subscription
	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "fanout",
		MinIntervalSeconds: 30,
	}
	_, _ = repo.Create(context.Background(), sub, 3600)

	// Notify (will try to send HTTP request but that's async)
	ctx := context.Background()
	svc.NotifyQueueReady(ctx, domain.CmdGenerateMaster)

	// Just verify it doesn't panic
	time.Sleep(100 * time.Millisecond)
}
