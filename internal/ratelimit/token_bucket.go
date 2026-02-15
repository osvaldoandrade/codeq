package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
)

type Bucket struct {
	RequestsPerMinute int `yaml:"requestsPerMinute"`
	BurstSize         int `yaml:"burstSize"`
}

func (b Bucket) Enabled() bool {
	return b.RequestsPerMinute > 0 && b.BurstSize > 0
}

type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

type Limiter interface {
	Allow(ctx context.Context, scope string, subject string, bucket Bucket) (Decision, error)
}

type TokenBucketLimiter struct {
	rdb *redis.Client
}

func NewTokenBucketLimiter(rdb *redis.Client) *TokenBucketLimiter {
	return &TokenBucketLimiter{rdb: rdb}
}

var tokenBucketScript = redis.NewScript(`
local key = KEYS[1]
local rate = tonumber(ARGV[1]) -- tokens/sec
local capacity = tonumber(ARGV[2])
local now = tonumber(ARGV[3]) -- ms
local ttl_ms = tonumber(ARGV[4])

local tokens = tonumber(redis.call("HGET", key, "tokens"))
local ts = tonumber(redis.call("HGET", key, "ts"))

if not tokens then tokens = capacity end
if not ts then ts = now end

if now < ts then ts = now end

local elapsed = now - ts
local refill = elapsed * (rate / 1000.0)
tokens = math.min(capacity, tokens + refill)

local allowed = 0
local retry_after_s = 0
if tokens >= 1.0 then
  allowed = 1
  tokens = tokens - 1.0
else
  allowed = 0
  if rate > 0 then
    local needed = 1.0 - tokens
    retry_after_s = math.ceil(needed / rate)
    if retry_after_s < 1 then retry_after_s = 1 end
  else
    retry_after_s = 60
  end
end

redis.call("HSET", key, "tokens", tokens, "ts", now)
redis.call("PEXPIRE", key, ttl_ms)
return {allowed, retry_after_s}
`)

func (l *TokenBucketLimiter) Allow(ctx context.Context, scope string, subject string, bucket Bucket) (Decision, error) {
	if l == nil || l.rdb == nil || !bucket.Enabled() {
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
	key := fmt.Sprintf("codeq:rl:%s:%s", scope, sha256Hex(subject))

	ratePerSec := float64(bucket.RequestsPerMinute) / 60.0
	capacity := float64(bucket.BurstSize)
	nowMS := time.Now().UTC().UnixMilli()

	// Expire the bucket state after a couple of "refill-to-full" cycles to bound memory.
	ttlMS := computeTTLMS(ratePerSec, capacity)

	res, err := tokenBucketScript.Run(ctx, l.rdb, []string{key}, ratePerSec, capacity, nowMS, ttlMS).Result()
	if err != nil {
		return Decision{}, err
	}
	vals, ok := res.([]interface{})
	if !ok || len(vals) < 2 {
		return Decision{}, fmt.Errorf("unexpected redis ratelimit response: %T", res)
	}

	allowed, _ := vals[0].(int64)
	retryAfterS, _ := vals[1].(int64)
	if allowed == 1 {
		return Decision{Allowed: true}, nil
	}
	if retryAfterS <= 0 {
		retryAfterS = 1
	}
	return Decision{Allowed: false, RetryAfter: time.Duration(retryAfterS) * time.Second}, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func computeTTLMS(ratePerSec float64, capacity float64) int64 {
	// Default to 2 minutes when rate/capacity are invalid.
	const minTTL = 30 * time.Second
	const maxTTL = 1 * time.Hour

	if ratePerSec <= 0 || capacity <= 0 {
		return int64((2 * time.Minute).Milliseconds())
	}

	// Time to refill from empty to full, then keep it around for ~2 cycles.
	fillSeconds := capacity / ratePerSec
	ttl := time.Duration(math.Ceil(fillSeconds*2.0))*time.Second + 5*time.Second

	if ttl < minTTL {
		ttl = minTTL
	}
	if ttl > maxTTL {
		ttl = maxTTL
	}
	return ttl.Milliseconds()
}
