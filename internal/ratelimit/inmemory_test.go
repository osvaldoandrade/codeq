package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestInMemoryLimiter_Disabled(t *testing.T) {
	l := NewInMemoryLimiter()
	defer l.Close()
	dec, err := l.Allow(context.Background(), "producer", "subj", Bucket{})
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !dec.Allowed {
		t.Errorf("disabled bucket should always allow")
	}
}

func TestInMemoryLimiter_BlocksAfterBurst(t *testing.T) {
	l := NewInMemoryLimiter()
	defer l.Close()
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return frozen }

	b := Bucket{RequestsPerMinute: 60, BurstSize: 3} // 1 token/sec, capacity 3
	// Consume the burst.
	for i := 0; i < 3; i++ {
		dec, _ := l.Allow(context.Background(), "producer", "subj", b)
		if !dec.Allowed {
			t.Errorf("burst %d: want allowed, got blocked", i)
		}
	}
	// Next one should block.
	dec, _ := l.Allow(context.Background(), "producer", "subj", b)
	if dec.Allowed {
		t.Errorf("post-burst: want blocked, got allowed")
	}
	if dec.RetryAfter < time.Second {
		t.Errorf("RetryAfter %v < 1s", dec.RetryAfter)
	}
}

func TestInMemoryLimiter_RefillsAfterWindow(t *testing.T) {
	l := NewInMemoryLimiter()
	defer l.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	b := Bucket{RequestsPerMinute: 60, BurstSize: 2} // 1/sec, capacity 2
	// Drain.
	_, _ = l.Allow(context.Background(), "producer", "subj", b)
	_, _ = l.Allow(context.Background(), "producer", "subj", b)
	dec, _ := l.Allow(context.Background(), "producer", "subj", b)
	if dec.Allowed {
		t.Fatal("expected blocked after drain")
	}
	// Advance 2 seconds → refill 2 tokens → allow 2 more.
	now = now.Add(2 * time.Second)
	for i := 0; i < 2; i++ {
		dec, _ := l.Allow(context.Background(), "producer", "subj", b)
		if !dec.Allowed {
			t.Errorf("refill %d: want allowed, got blocked (RetryAfter=%v)", i, dec.RetryAfter)
		}
	}
}

func TestInMemoryLimiter_IsolatesPerSubject(t *testing.T) {
	l := NewInMemoryLimiter()
	defer l.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	b := Bucket{RequestsPerMinute: 60, BurstSize: 1}

	// Subject A consumes its single token.
	if dec, _ := l.Allow(context.Background(), "producer", "subj-A", b); !dec.Allowed {
		t.Fatal("subj-A first call should be allowed")
	}
	if dec, _ := l.Allow(context.Background(), "producer", "subj-A", b); dec.Allowed {
		t.Error("subj-A second call should be blocked")
	}
	// Subject B has its own token.
	if dec, _ := l.Allow(context.Background(), "producer", "subj-B", b); !dec.Allowed {
		t.Error("subj-B should be unaffected by subj-A's drain")
	}
}

func TestInMemoryLimiter_IsolatesPerScope(t *testing.T) {
	l := NewInMemoryLimiter()
	defer l.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	b := Bucket{RequestsPerMinute: 60, BurstSize: 1}

	// Same subject, different scopes → independent buckets.
	if dec, _ := l.Allow(context.Background(), "producer", "x", b); !dec.Allowed {
		t.Fatal("producer scope first call")
	}
	if dec, _ := l.Allow(context.Background(), "producer", "x", b); dec.Allowed {
		t.Error("producer scope second call should block")
	}
	if dec, _ := l.Allow(context.Background(), "worker", "x", b); !dec.Allowed {
		t.Error("worker scope should be independent")
	}
}

func TestInMemoryLimiter_CapacityChangeClamps(t *testing.T) {
	l := NewInMemoryLimiter()
	defer l.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	big := Bucket{RequestsPerMinute: 60, BurstSize: 100}
	small := Bucket{RequestsPerMinute: 60, BurstSize: 2}

	// Initialize bucket at capacity=100 (use 1 token to register state).
	_, _ = l.Allow(context.Background(), "producer", "subj", big)
	// Reconfigure to capacity=2 — token count should clamp.
	for i := 0; i < 2; i++ {
		dec, _ := l.Allow(context.Background(), "producer", "subj", small)
		if !dec.Allowed {
			t.Errorf("after clamp, call %d should still allow", i)
		}
	}
	dec, _ := l.Allow(context.Background(), "producer", "subj", small)
	if dec.Allowed {
		t.Error("after clamp + drain, should block")
	}
}

func TestInMemoryLimiter_ConcurrentSameSubject(t *testing.T) {
	l := NewInMemoryLimiter()
	defer l.Close()

	b := Bucket{RequestsPerMinute: 60_000, BurstSize: 1000}

	var allowed, blocked int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	const concurrency = 32
	const perGoroutine = 100
	for g := 0; g < concurrency; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				dec, _ := l.Allow(context.Background(), "producer", "shared-subj", b)
				mu.Lock()
				if dec.Allowed {
					allowed++
				} else {
					blocked++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	total := allowed + blocked
	if total != int64(concurrency*perGoroutine) {
		t.Fatalf("total calls: got %d, want %d", total, concurrency*perGoroutine)
	}
	if allowed > int64(b.BurstSize)+50 { // small slop for refill during the test
		t.Errorf("allowed=%d exceeds burst capacity %d by more than slop", allowed, b.BurstSize)
	}
}

func TestInMemoryLimiter_JanitorReapsIdle(t *testing.T) {
	l := NewInMemoryLimiter()
	defer l.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	b := Bucket{RequestsPerMinute: 60, BurstSize: 1}
	_, _ = l.Allow(context.Background(), "producer", "subj", b)

	if len(l.buckets) != 1 {
		t.Fatalf("seed: want 1 bucket, got %d", len(l.buckets))
	}

	// Idle threshold inside gcOnce is 10 minutes; advance 15.
	now = now.Add(15 * time.Minute)
	l.gcOnce(10 * time.Minute)
	if len(l.buckets) != 0 {
		t.Errorf("post-GC: want 0 buckets, got %d", len(l.buckets))
	}
}
