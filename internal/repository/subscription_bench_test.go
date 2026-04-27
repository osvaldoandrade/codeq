package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

// BenchmarkSubscription_ListActive_WithoutPipeline simulates the old sequential behavior
// for comparison purposes. This shows the performance improvement from pipelining.
func BenchmarkSubscription_ListActive(b *testing.B) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatalf("miniredis start: %v", err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	repo := NewSubscriptionRepository(client, time.UTC)
	ctx := context.Background()

	// Setup: Create multiple subscriptions for a command
	now := time.Now().In(time.UTC)
	cmd := domain.CmdGenerateMaster
	numSubs := 100

	for i := 0; i < numSubs; i++ {
		sub := domain.Subscription{
			ID:                 fmt.Sprintf("sub-%d", i),
			EventTypes:         []domain.Command{cmd},
			CallbackURL:        "http://example.com/webhook",
			MinIntervalSeconds: 5,
		}
		_, err := repo.Create(ctx, sub, 300)
		if err != nil {
			b.Fatalf("Create subscription: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := repo.ListActive(ctx, cmd, now)
		if err != nil {
			b.Fatalf("ListActive: %v", err)
		}
	}
}

// BenchmarkSubscription_ListActive_Scaled tests with larger subscription counts
// to better demonstrate the pipelining benefit (more noticeable with N+1 problems)
func BenchmarkSubscription_ListActive_Scaled(b *testing.B) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatalf("miniredis start: %v", err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	repo := NewSubscriptionRepository(client, time.UTC)
	ctx := context.Background()

	// Setup: Create many subscriptions for a command (more realistic scenario)
	now := time.Now().In(time.UTC)
	cmd := domain.CmdGenerateMaster
	numSubs := 500

	for i := 0; i < numSubs; i++ {
		sub := domain.Subscription{
			ID:                 fmt.Sprintf("sub-large-%d", i),
			EventTypes:         []domain.Command{cmd},
			CallbackURL:        "http://example.com/webhook",
			MinIntervalSeconds: 5,
		}
		_, err := repo.Create(ctx, sub, 300)
		if err != nil {
			b.Fatalf("Create subscription: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := repo.ListActive(ctx, cmd, now)
		if err != nil {
			b.Fatalf("ListActive: %v", err)
		}
	}
}
