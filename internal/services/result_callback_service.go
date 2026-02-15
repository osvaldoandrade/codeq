package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

type ResultCallbackService interface {
	Send(ctx context.Context, task domain.Task, rec domain.ResultRecord)
}

type resultCallbackService struct {
	logger      *slog.Logger
	secret      string
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration

	limiter ratelimit.Limiter
	bucket  ratelimit.Bucket
}

func NewResultCallbackService(logger *slog.Logger, secret string, maxAttempts int, baseDelaySeconds int, maxDelaySeconds int, limiter ratelimit.Limiter, bucket ratelimit.Bucket) ResultCallbackService {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	if baseDelaySeconds <= 0 {
		baseDelaySeconds = 2
	}
	if maxDelaySeconds <= 0 {
		maxDelaySeconds = 60
	}
	return &resultCallbackService{
		logger:      logger,
		secret:      secret,
		maxAttempts: maxAttempts,
		baseDelay:   time.Duration(baseDelaySeconds) * time.Second,
		maxDelay:    time.Duration(maxDelaySeconds) * time.Second,
		limiter:     limiter,
		bucket:      bucket,
	}
}

func (s *resultCallbackService) Send(ctx context.Context, task domain.Task, rec domain.ResultRecord) {
	if strings.TrimSpace(task.Webhook) == "" {
		return
	}
	payload := map[string]any{
		"taskId":      task.ID,
		"eventType":   string(task.Command),
		"status":      rec.Status,
		"result":      rec.Result,
		"error":       rec.Error,
		"artifacts":   rec.Artifacts,
		"completedAt": rec.CompletedAt,
	}

	b, _ := json.Marshal(payload)
	go s.sendWithRetry(ctx, task.Command, task.Webhook, b)
}

func (s *resultCallbackService) sendWithRetry(ctx context.Context, cmd domain.Command, url string, body []byte) {
	for attempt := 1; attempt <= s.maxAttempts; attempt++ {
		if s.limiter != nil && s.bucket.Enabled() {
			for {
				dec, err := s.limiter.Allow(ctx, "webhook", url, s.bucket)
				if err != nil {
					// Fail open.
					break
				}
				if dec.Allowed {
					break
				}
				metrics.RateLimitHitsTotal.WithLabelValues("webhook", "task_result").Inc()
				if sleepOrDone(ctx, dec.RetryAfter) != nil {
					return
				}
			}
		}

		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		s.addSignature(req, body)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			_ = resp.Body.Close()
			metrics.WebhookDeliveriesTotal.WithLabelValues("task_result", string(cmd), "success").Inc()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		delay := s.backoffDelay(attempt)
		_ = sleepOrDone(ctx, delay)
	}
	metrics.WebhookDeliveriesTotal.WithLabelValues("task_result", string(cmd), "failure").Inc()
	s.logger.Warn("result callback failed", "url", url)
}

func (s *resultCallbackService) backoffDelay(attempt int) time.Duration {
	d := s.baseDelay * time.Duration(1<<uint(attempt-1))
	if d > s.maxDelay {
		d = s.maxDelay
	}
	return d
}

func sleepOrDone(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *resultCallbackService) addSignature(req *http.Request, body []byte) {
	if strings.TrimSpace(s.secret) == "" {
		return
	}
	ts := time.Now().UTC().Unix()
	mac := hmac.New(sha256.New, []byte(s.secret))
	_, _ = mac.Write([]byte(fmt.Sprintf("%d.", ts)))
	_, _ = mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	req.Header.Set("X-CodeQ-Timestamp", fmt.Sprintf("%d", ts))
	req.Header.Set("X-CodeQ-Signature", sig)
}
