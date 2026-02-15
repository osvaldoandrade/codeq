package ratelimit

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestTokenBucketLimiter_Allow_Disabled(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	lim := NewTokenBucketLimiter(rdb)

	dec, err := lim.Allow(context.Background(), "producer", "user-1", Bucket{})
	if err != nil {
		t.Fatalf("allow: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("expected allowed when bucket disabled")
	}
}

func TestTokenBucketLimiter_Allow_BlocksAfterBurst(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	lim := NewTokenBucketLimiter(rdb)
	bucket := Bucket{RequestsPerMinute: 60, BurstSize: 1} // 1 token/sec, burst=1

	dec1, err := lim.Allow(context.Background(), "producer", "user-1", bucket)
	if err != nil {
		t.Fatalf("allow 1: %v", err)
	}
	if !dec1.Allowed {
		t.Fatalf("expected first request to be allowed")
	}

	dec2, err := lim.Allow(context.Background(), "producer", "user-1", bucket)
	if err != nil {
		t.Fatalf("allow 2: %v", err)
	}
	if dec2.Allowed {
		t.Fatalf("expected second request to be rate limited")
	}
	if dec2.RetryAfter <= 0 {
		t.Fatalf("expected retryAfter to be set")
	}

	decOther, err := lim.Allow(context.Background(), "producer", "user-2", bucket)
	if err != nil {
		t.Fatalf("allow other: %v", err)
	}
	if !decOther.Allowed {
		t.Fatalf("expected other subject to be allowed (independent bucket)")
	}
}
