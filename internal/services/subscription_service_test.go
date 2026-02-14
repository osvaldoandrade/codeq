package services

import (
	"context"
	"testing"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"time"
)

func setupSubscriptionServiceTest(t *testing.T) (context.Context, SubscriptionService) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis start: %v", err)
	}
	t.Cleanup(mr.Close)
	
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	
	repo := repository.NewSubscriptionRepository(rdb, time.UTC)
	svc := NewSubscriptionService(repo)
	
	return context.Background(), svc
}

func TestSubscriptionServiceCreateSuccess(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	sub, err := svc.Create(ctx, "https://example.com/callback", []domain.Command{domain.CmdGenerateMaster}, "fanout", "", 3600, 30)
	
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if sub == nil {
		t.Fatal("Expected subscription to be non-nil")
	}
	if sub.CallbackURL != "https://example.com/callback" {
		t.Errorf("Expected callback URL, got %s", sub.CallbackURL)
	}
}

func TestSubscriptionServiceCreateEmptyURL(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	_, err := svc.Create(ctx, "", []domain.Command{domain.CmdGenerateMaster}, "fanout", "", 3600, 30)
	
	if err == nil {
		t.Fatal("Expected error for empty callback URL")
	}
	if err.Error() != "callbackUrl is required" {
		t.Errorf("Expected 'callbackUrl is required', got %v", err)
	}
}

func TestSubscriptionServiceCreateInvalidURL(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	tests := []struct {
		name string
		url  string
	}{
		{"invalid url", "not-a-url"},
		{"ftp scheme", "ftp://example.com"},
		{"no host", "http://"},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Create(ctx, tt.url, []domain.Command{domain.CmdGenerateMaster}, "fanout", "", 3600, 30)
			if err == nil {
				t.Fatal("Expected error for invalid callback URL")
			}
			if err.Error() != "invalid callback url" {
				t.Errorf("Expected 'invalid callback url', got %v", err)
			}
		})
	}
}

func TestSubscriptionServiceCreateDefaultEventTypes(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	sub, err := svc.Create(ctx, "https://example.com/callback", []domain.Command{}, "fanout", "", 3600, 30)
	
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if len(sub.EventTypes) != 2 {
		t.Errorf("Expected 2 default event types, got %d", len(sub.EventTypes))
	}
}

func TestSubscriptionServiceCreateDefaultDeliveryMode(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	sub, err := svc.Create(ctx, "https://example.com/callback", []domain.Command{domain.CmdGenerateMaster}, "", "", 3600, 30)
	
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if sub.DeliveryMode != "fanout" {
		t.Errorf("Expected default delivery mode 'fanout', got %s", sub.DeliveryMode)
	}
}

func TestSubscriptionServiceCreateInvalidDeliveryMode(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	_, err := svc.Create(ctx, "https://example.com/callback", []domain.Command{domain.CmdGenerateMaster}, "invalid-mode", "", 3600, 30)
	
	if err == nil {
		t.Fatal("Expected error for invalid delivery mode")
	}
	if err.Error() != "invalid deliveryMode" {
		t.Errorf("Expected 'invalid deliveryMode', got %v", err)
	}
}

func TestSubscriptionServiceCreateGroupModeWithoutGroupID(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	_, err := svc.Create(ctx, "https://example.com/callback", []domain.Command{domain.CmdGenerateMaster}, "group", "", 3600, 30)
	
	if err == nil {
		t.Fatal("Expected error for group mode without groupId")
	}
	if err.Error() != "groupId is required" {
		t.Errorf("Expected 'groupId is required', got %v", err)
	}
}

func TestSubscriptionServiceCreateGroupModeWithGroupID(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	sub, err := svc.Create(ctx, "https://example.com/callback", []domain.Command{domain.CmdGenerateMaster}, "group", "worker-group-1", 3600, 30)
	
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if sub.DeliveryMode != "group" {
		t.Errorf("Expected delivery mode 'group', got %s", sub.DeliveryMode)
	}
	if sub.GroupID != "worker-group-1" {
		t.Errorf("Expected groupID 'worker-group-1', got %s", sub.GroupID)
	}
}

func TestSubscriptionServiceCreateHashMode(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	sub, err := svc.Create(ctx, "https://example.com/callback", []domain.Command{domain.CmdGenerateMaster}, "hash", "", 3600, 30)
	
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if sub.DeliveryMode != "hash" {
		t.Errorf("Expected delivery mode 'hash', got %s", sub.DeliveryMode)
	}
}

func TestSubscriptionServiceHeartbeat(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	// Create a subscription first
	sub, _ := svc.Create(ctx, "https://example.com/callback", []domain.Command{domain.CmdGenerateMaster}, "fanout", "", 3600, 30)
	
	// Heartbeat
	updated, err := svc.Heartbeat(ctx, sub.ID, 7200)
	
	if err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}
	if updated == nil {
		t.Fatal("Expected updated subscription to be non-nil")
	}
	if updated.ID != sub.ID {
		t.Errorf("Expected ID %s, got %s", sub.ID, updated.ID)
	}
}

func TestSubscriptionServiceHeartbeatNotFound(t *testing.T) {
	ctx, svc := setupSubscriptionServiceTest(t)
	
	_, err := svc.Heartbeat(ctx, "nonexistent-id", 3600)
	
	if err == nil {
		t.Fatal("Expected error for nonexistent subscription")
	}
}
