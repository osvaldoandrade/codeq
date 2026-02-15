package middleware

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

func RateLimitProducer(lim ratelimit.Limiter, cfg *config.Config) gin.HandlerFunc {
	return rateLimitBearer(lim, "producer", "create_task", cfg.RateLimit.Producer)
}

func RateLimitWorkerClaim(lim ratelimit.Limiter, cfg *config.Config) gin.HandlerFunc {
	return rateLimitBearer(lim, "worker", "claim", cfg.RateLimit.Worker)
}

func RateLimitAdminCleanup(lim ratelimit.Limiter, cfg *config.Config) gin.HandlerFunc {
	return rateLimitBearer(lim, "admin", "cleanup", cfg.RateLimit.Admin)
}

func rateLimitBearer(lim ratelimit.Limiter, scope string, operation string, bcfg config.RateLimitBucketConfig) gin.HandlerFunc {
	bucket := ratelimit.Bucket{RequestsPerMinute: bcfg.RequestsPerMinute, BurstSize: bcfg.BurstSize}
	return func(c *gin.Context) {
		if lim == nil || !bucket.Enabled() {
			c.Next()
			return
		}

		token := bearerToken(c.GetHeader("Authorization"))
		if token == "" {
			// Auth middleware will reject; don't rate limit unauthenticated requests here.
			c.Next()
			return
		}

		dec, err := lim.Allow(c.Request.Context(), scope, token, bucket)
		if err != nil {
			// Fail open to avoid turning Redis hiccups into outages.
			slog.Default().Warn("rate limit check failed", "scope", scope, "op", operation, "err", err)
			c.Next()
			return
		}
		if dec.Allowed {
			c.Next()
			return
		}

		retryAfterSeconds := int(dec.RetryAfter.Seconds())
		if retryAfterSeconds <= 0 {
			retryAfterSeconds = 1
		}
		c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
		metrics.RateLimitHitsTotal.WithLabelValues(scope, operation).Inc()
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error":             "rate limit exceeded",
			"scope":             scope,
			"operation":         operation,
			"retryAfterSeconds": retryAfterSeconds,
		})
	}
}

func bearerToken(authHeader string) string {
	authHeader = strings.TrimSpace(authHeader)
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
