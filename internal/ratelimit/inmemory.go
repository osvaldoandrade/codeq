package ratelimit

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// InMemoryLimiter is a process-local token-bucket rate limiter. Each
// (scope, subject) pair gets its own bucket. There is no
// cross-process coordination: in a cluster of N codeq nodes the
// effective per-tenant ceiling is N × bucket.RequestsPerMinute. That
// is acceptable for typical codeq deployments (legitimate clients
// don't get rebalanced across nodes mid-burst), and avoids dragging a
// Redis dependency into the pure-Pebble path.
//
// Without this, the pebble code path used a noopLimiter that allowed
// everything — silent foot-gun if operators configured rate limits
// expecting them to work. This limiter ENFORCES the configured limits
// per-node so the configuration takes effect.
//
// Concurrency model: a top-level mutex protects buckets map; each
// bucket carries its own mutex for the refill+decrement critical
// section. For typical workloads with one bucket per JWT subject the
// per-bucket mutex never contends.
//
// Bucket GC: a background loop reaps buckets idle longer than 2×
// refill-to-full window so the map stays bounded under churn (many
// short-lived tokens).
type InMemoryLimiter struct {
	mu      sync.RWMutex
	buckets map[string]*memBucketState
	now     func() time.Time

	stopOnce sync.Once
	stopCh   chan struct{}
}

type memBucketState struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	lastTS   time.Time
}

// NewInMemoryLimiter constructs an in-memory limiter and starts a
// janitor goroutine that reaps idle buckets. Call Close on shutdown
// to stop the janitor.
func NewInMemoryLimiter() *InMemoryLimiter {
	l := &InMemoryLimiter{
		buckets: make(map[string]*memBucketState),
		now:     time.Now,
		stopCh:  make(chan struct{}),
	}
	go l.janitor()
	return l
}

// Close stops the janitor goroutine. Idempotent.
func (l *InMemoryLimiter) Close() error {
	if l == nil {
		return nil
	}
	l.stopOnce.Do(func() { close(l.stopCh) })
	return nil
}

// Allow consumes one token from the bucket identified by (scope,
// subject). Returns Decision{Allowed: true} when a token was
// available; otherwise Decision{Allowed: false, RetryAfter: d} where
// d is the minimum wait until a token would be available.
func (l *InMemoryLimiter) Allow(ctx context.Context, scope string, subject string, bucket Bucket) (Decision, error) {
	if l == nil || !bucket.Enabled() {
		return Decision{Allowed: true}, nil
	}
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = "default"
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = "unknown"
	}
	key := fmt.Sprintf("%s:%s", scope, sha256Hex(subject))

	ratePerSec := float64(bucket.RequestsPerMinute) / 60.0
	capacity := float64(bucket.BurstSize)

	state := l.bucketFor(key, capacity)
	state.mu.Lock()
	defer state.mu.Unlock()

	// Capacity can change on the fly (operator reconfig). Clamp tokens
	// to the new ceiling on every Allow so a downward change takes
	// effect without restart.
	if state.capacity != capacity {
		state.capacity = capacity
		if state.tokens > capacity {
			state.tokens = capacity
		}
	}

	now := l.now()
	elapsed := now.Sub(state.lastTS).Seconds()
	if elapsed > 0 {
		state.tokens = math.Min(capacity, state.tokens+elapsed*ratePerSec)
	}
	state.lastTS = now

	if state.tokens >= 1.0 {
		state.tokens -= 1.0
		_ = ctx
		return Decision{Allowed: true}, nil
	}
	// Not enough tokens. Compute the wait until the bucket refills
	// enough for one token; round up to the next second so clients
	// don't poll busy.
	var retryAfter time.Duration
	if ratePerSec > 0 {
		needed := 1.0 - state.tokens
		secs := math.Ceil(needed / ratePerSec)
		if secs < 1 {
			secs = 1
		}
		retryAfter = time.Duration(secs) * time.Second
	} else {
		retryAfter = 60 * time.Second
	}
	return Decision{Allowed: false, RetryAfter: retryAfter}, nil
}

func (l *InMemoryLimiter) bucketFor(key string, capacity float64) *memBucketState {
	l.mu.RLock()
	state, ok := l.buckets[key]
	l.mu.RUnlock()
	if ok {
		return state
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if state, ok = l.buckets[key]; ok {
		return state
	}
	state = &memBucketState{
		tokens:   capacity,
		capacity: capacity,
		lastTS:   l.now(),
	}
	l.buckets[key] = state
	return state
}

// janitor reaps buckets that haven't been touched in idleThreshold.
// Keeps the map bounded under per-token (per-subject) churn.
func (l *InMemoryLimiter) janitor() {
	const (
		tick          = 5 * time.Minute
		idleThreshold = 10 * time.Minute
	)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-l.stopCh:
			return
		case <-t.C:
			l.gcOnce(idleThreshold)
		}
	}
}

func (l *InMemoryLimiter) gcOnce(idleThreshold time.Duration) {
	cutoff := l.now().Add(-idleThreshold)
	l.mu.Lock()
	for key, state := range l.buckets {
		state.mu.Lock()
		idle := state.lastTS.Before(cutoff)
		state.mu.Unlock()
		if idle {
			delete(l.buckets, key)
		}
	}
	l.mu.Unlock()
}

// Compile-time check.
var _ Limiter = (*InMemoryLimiter)(nil)
