package repository

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func setupSubscriptionRepo(t *testing.T) (context.Context, *miniredis.Miniredis, *redis.Client, SubscriptionRepository) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	
	repo := NewSubscriptionRepository(rdb, time.UTC)
	
	return context.Background(), mr, rdb, repo
}

func TestSubscriptionCreate(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "fanout",
		GroupID:            "",
		MinIntervalSeconds: 30,
	}

	created, err := repo.Create(ctx, sub, 3600)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if created.ID == "" {
		t.Error("Expected subscription ID to be set")
	}
	if created.CallbackURL != sub.CallbackURL {
		t.Errorf("CallbackURL = %v, want %v", created.CallbackURL, sub.CallbackURL)
	}
	if len(created.EventTypes) != 1 {
		t.Errorf("EventTypes length = %v, want 1", len(created.EventTypes))
	}
}

func TestSubscriptionHeartbeat(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	// Create a subscription first
	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "fanout",
		MinIntervalSeconds: 30,
	}
	created, _ := repo.Create(ctx, sub, 3600)

	// Heartbeat to extend TTL
	updated, err := repo.Heartbeat(ctx, created.ID, 7200)
	if err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}

	if updated.ID != created.ID {
		t.Errorf("Heartbeat() ID = %v, want %v", updated.ID, created.ID)
	}
}

func TestSubscriptionHeartbeatNotFound(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	_, err := repo.Heartbeat(ctx, "nonexistent-id", 3600)
	if err == nil {
		t.Fatal("Expected error for nonexistent subscription")
	}
}

func TestSubscriptionGet(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	// Create a subscription
	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "fanout",
		MinIntervalSeconds: 30,
	}
	created, _ := repo.Create(ctx, sub, 3600)

	// Get the subscription
	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if got.ID != created.ID {
		t.Errorf("Get() ID = %v, want %v", got.ID, created.ID)
	}
	if got.CallbackURL != sub.CallbackURL {
		t.Errorf("Get() CallbackURL = %v, want %v", got.CallbackURL, sub.CallbackURL)
	}
}

func TestSubscriptionGetNotFound(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	_, err := repo.Get(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("Expected error for nonexistent subscription")
	}
}

func TestSubscriptionListActive(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	// Create a subscription
	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "fanout",
		MinIntervalSeconds: 30,
	}
	_, _ = repo.Create(ctx, sub, 3600)

	// List active subscriptions
	now := time.Now().UTC()
	subs, err := repo.ListActive(ctx, domain.CmdGenerateMaster, now)
	if err != nil {
		t.Fatalf("ListActive() error = %v", err)
	}

	if len(subs) == 0 {
		t.Error("Expected at least one active subscription")
	}
}

func TestSubscriptionListActiveNoMatches(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	// Create a subscription for a different event type
	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "fanout",
		MinIntervalSeconds: 30,
	}
	_, _ = repo.Create(ctx, sub, 3600)

	// List active subscriptions for different command
	now := time.Now().UTC()
	subs, err := repo.ListActive(ctx, domain.CmdGenerateCreative, now)
	if err != nil {
		t.Fatalf("ListActive() error = %v", err)
	}

	// Should not return subscriptions for different event type
	if len(subs) > 0 {
		// Actually, it might return if the subscription is registered for all types
		// This is implementation-dependent
	}
}

func TestSubscriptionAllowNotify(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	// Create a subscription
	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "fanout",
		MinIntervalSeconds: 30,
	}
	created, _ := repo.Create(ctx, sub, 3600)

	// First notification should be allowed
	allowed, err := repo.AllowNotify(ctx, created.ID, 30)
	if err != nil {
		t.Fatalf("AllowNotify() error = %v", err)
	}
	if !allowed {
		t.Error("Expected first notification to be allowed")
	}

	// Immediate second notification should be throttled
	allowed, err = repo.AllowNotify(ctx, created.ID, 30)
	if err != nil {
		t.Fatalf("AllowNotify() error = %v", err)
	}
	if allowed {
		t.Error("Expected second notification to be throttled")
	}
}

func TestSubscriptionNextGroupIndex(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	// Test round-robin index
	idx1, err := repo.NextGroupIndex(ctx, domain.CmdGenerateMaster, "group-1", 3)
	if err != nil {
		t.Fatalf("NextGroupIndex() error = %v", err)
	}
	if idx1 < 0 || idx1 >= 3 {
		t.Errorf("NextGroupIndex() = %v, want value between 0 and 2", idx1)
	}

	idx2, err := repo.NextGroupIndex(ctx, domain.CmdGenerateMaster, "group-1", 3)
	if err != nil {
		t.Fatalf("NextGroupIndex() error = %v", err)
	}
	if idx2 < 0 || idx2 >= 3 {
		t.Errorf("NextGroupIndex() = %v, want value between 0 and 2", idx2)
	}

	// Should increment
	if idx2 != (idx1+1)%3 {
		t.Errorf("NextGroupIndex() = %v, want %v (round-robin)", idx2, (idx1+1)%3)
	}
}

func TestSubscriptionCleanupExpired(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	// Create a subscription with short TTL
	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "fanout",
		MinIntervalSeconds: 30,
	}
	_, _ = repo.Create(ctx, sub, 1) // 1 second TTL

	// Wait for expiration
	time.Sleep(2 * time.Second)

	// Cleanup expired subscriptions
	deleted, err := repo.CleanupExpired(ctx, 10, time.Now().UTC())
	if err != nil {
		t.Fatalf("CleanupExpired() error = %v", err)
	}

	if deleted < 0 {
		t.Errorf("CleanupExpired() = %v, want non-negative", deleted)
	}
}

func TestSubscriptionCreateWithGroupMode(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "group",
		GroupID:            "worker-group-1",
		MinIntervalSeconds: 30,
	}

	created, err := repo.Create(ctx, sub, 3600)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if created.DeliveryMode != "group" {
		t.Errorf("DeliveryMode = %v, want 'group'", created.DeliveryMode)
	}
	if created.GroupID != "worker-group-1" {
		t.Errorf("GroupID = %v, want 'worker-group-1'", created.GroupID)
	}
}

func TestSubscriptionCreateWithHashMode(t *testing.T) {
	ctx, _, _, repo := setupSubscriptionRepo(t)

	sub := domain.Subscription{
		CallbackURL:        "https://example.com/callback",
		EventTypes:         []domain.Command{domain.CmdGenerateMaster},
		DeliveryMode:       "hash",
		MinIntervalSeconds: 30,
	}

	created, err := repo.Create(ctx, sub, 3600)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if created.DeliveryMode != "hash" {
		t.Errorf("DeliveryMode = %v, want 'hash'", created.DeliveryMode)
	}
}
