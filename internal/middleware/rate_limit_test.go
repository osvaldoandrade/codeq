package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// mockLimiter implements ratelimit.Limiter for testing
type mockLimiter struct {
	decision ratelimit.Decision
	err      error
}

func (m *mockLimiter) Allow(ctx context.Context, scope string, subject string, bucket ratelimit.Bucket) (ratelimit.Decision, error) {
	return m.decision, m.err
}

func TestRateLimitProducer_DisabledBucket(t *testing.T) {
	cfg := &config.Config{
		RateLimit: config.RateLimitConfig{
			Producer: config.RateLimitBucketConfig{
				RequestsPerMinute: 0, // disabled
				BurstSize:         0,
			},
		},
	}

	limiter := &mockLimiter{
		decision: ratelimit.Decision{Allowed: false}, // Should not be called
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/codeq/tasks", nil)
	ctx.Request.Header.Set("Authorization", "Bearer test-token")

	RateLimitProducer(limiter, cfg)(ctx)

	// Should pass through (not abort)
	if ctx.IsAborted() {
		t.Fatal("expected request to pass through for disabled bucket")
	}
}

func TestRateLimitProducer_AllowedDecision(t *testing.T) {
	cfg := &config.Config{
		RateLimit: config.RateLimitConfig{
			Producer: config.RateLimitBucketConfig{
				RequestsPerMinute: 100,
				BurstSize:         10,
			},
		},
	}

	limiter := &mockLimiter{
		decision: ratelimit.Decision{Allowed: true},
		err:      nil,
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/codeq/tasks", nil)
	ctx.Request.Header.Set("Authorization", "Bearer test-token")

	RateLimitProducer(limiter, cfg)(ctx)

	if ctx.IsAborted() {
		t.Fatal("expected request to pass through when rate limit allows")
	}
}

func TestRateLimitProducer_DeniedDecision(t *testing.T) {
	cfg := &config.Config{
		RateLimit: config.RateLimitConfig{
			Producer: config.RateLimitBucketConfig{
				RequestsPerMinute: 100,
				BurstSize:         10,
			},
		},
	}

	limiter := &mockLimiter{
		decision: ratelimit.Decision{
			Allowed:    false,
			RetryAfter: 5 * time.Second,
		},
		err: nil,
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/codeq/tasks", nil)
	ctx.Request.Header.Set("Authorization", "Bearer test-token")

	RateLimitProducer(limiter, cfg)(ctx)

	if !ctx.IsAborted() {
		t.Fatal("expected request to be aborted when rate limited")
	}

	// Check response code
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 status, got %d", rec.Code)
	}

	// Check Retry-After header
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter != "5" {
		t.Fatalf("expected Retry-After: 5, got %s", retryAfter)
	}

	// Check JSON response
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to unmarshal JSON response: %v", err)
	}

	if body["error"] != "rate limit exceeded" {
		t.Fatalf("expected error field, got %v", body)
	}
	if body["scope"] != "producer" {
		t.Fatalf("expected scope=producer, got %v", body["scope"])
	}
	if body["operation"] != "create_task" {
		t.Fatalf("expected operation=create_task, got %v", body["operation"])
	}
	if body["retryAfterSeconds"] != float64(5) {
		t.Fatalf("expected retryAfterSeconds=5, got %v", body["retryAfterSeconds"])
	}

	// Note: Metric increment verification would require access to the metrics registry
	// In production code, we'd typically use prometheus.NewRegistry() for test isolation
}

func TestRateLimitWorkerClaim_RedisError(t *testing.T) {
	cfg := &config.Config{
		RateLimit: config.RateLimitConfig{
			Worker: config.RateLimitBucketConfig{
				RequestsPerMinute: 100,
				BurstSize:         10,
			},
		},
	}

	limiter := &mockLimiter{
		decision: ratelimit.Decision{Allowed: false},
		err:      context.DeadlineExceeded, // Simulate Redis error
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/codeq/tasks/claim", nil)
	ctx.Request.Header.Set("Authorization", "Bearer worker-token")

	RateLimitWorkerClaim(limiter, cfg)(ctx)

	// Should fail open - allow request to proceed
	if ctx.IsAborted() {
		t.Fatal("expected request to pass through when limiter returns error (fail open)")
	}
}

func TestRateLimitProducer_NoAuthHeader(t *testing.T) {
	cfg := &config.Config{
		RateLimit: config.RateLimitConfig{
			Producer: config.RateLimitBucketConfig{
				RequestsPerMinute: 100,
				BurstSize:         10,
			},
		},
	}

	limiter := &mockLimiter{
		decision: ratelimit.Decision{Allowed: false}, // Should not be called
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/codeq/tasks", nil)
	// No Authorization header

	RateLimitProducer(limiter, cfg)(ctx)

	if ctx.IsAborted() {
		t.Fatal("unauthenticated requests should pass through")
	}
}

func TestRateLimitProducer_NilLimiter(t *testing.T) {
	cfg := &config.Config{
		RateLimit: config.RateLimitConfig{
			Producer: config.RateLimitBucketConfig{
				RequestsPerMinute: 100,
				BurstSize:         10,
			},
		},
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/codeq/tasks", nil)
	ctx.Request.Header.Set("Authorization", "Bearer test-token")

	RateLimitProducer(nil, cfg)(ctx)

	if ctx.IsAborted() {
		t.Fatal("expected request to pass through with nil limiter")
	}
}

func TestRateLimitAdminCleanup_DeniedWithRetryAfterLessThanOne(t *testing.T) {
	cfg := &config.Config{
		RateLimit: config.RateLimitConfig{
			Admin: config.RateLimitBucketConfig{
				RequestsPerMinute: 30,
				BurstSize:         5,
			},
		},
	}

	limiter := &mockLimiter{
		decision: ratelimit.Decision{
			Allowed:    false,
			RetryAfter: 500 * time.Millisecond, // Less than 1 second
		},
		err: nil,
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/codeq/admin/tasks/cleanup", nil)
	ctx.Request.Header.Set("Authorization", "Bearer admin-token")

	RateLimitAdminCleanup(limiter, cfg)(ctx)

	// Check that Retry-After is at least 1
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter != "1" {
		t.Fatalf("expected Retry-After: 1 (minimum), got %s", retryAfter)
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "valid bearer token",
			header: "Bearer abc123",
			want:   "abc123",
		},
		{
			name:   "valid with extra spaces",
			header: "  Bearer   def456  ",
			want:   "def456",
		},
		{
			name:   "case insensitive bearer",
			header: "bearer xyz789",
			want:   "xyz789",
		},
		{
			name:   "empty header",
			header: "",
			want:   "",
		},
		{
			name:   "missing token",
			header: "Bearer",
			want:   "",
		},
		{
			name:   "wrong scheme",
			header: "Basic abc123",
			want:   "",
		},
		{
			name:   "no scheme",
			header: "justtoken",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bearerToken(tt.header)
			if got != tt.want {
				t.Errorf("bearerToken(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}
