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
	urlpkg "net/url"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/internal/tracing"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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

	client *http.Client
}

func NewResultCallbackService(logger *slog.Logger, secret string, maxAttempts int, baseDelaySeconds int, maxDelaySeconds int, limiter ratelimit.Limiter, bucket ratelimit.Bucket, client *http.Client) ResultCallbackService {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	if baseDelaySeconds <= 0 {
		baseDelaySeconds = 2
	}
	if maxDelaySeconds <= 0 {
		maxDelaySeconds = 60
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &resultCallbackService{
		logger:      logger,
		secret:      secret,
		maxAttempts: maxAttempts,
		baseDelay:   time.Duration(baseDelaySeconds) * time.Second,
		maxDelay:    time.Duration(maxDelaySeconds) * time.Second,
		limiter:     limiter,
		bucket:      bucket,
		client:      client,
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
	go s.sendWithRetry(context.WithoutCancel(ctx), task.Command, task.Webhook, b)
}

func (s *resultCallbackService) sendWithRetry(ctx context.Context, cmd domain.Command, url string, body []byte) {
	host := ""
	if u, err := urlpkg.Parse(url); err == nil {
		host = u.Host
	}

	for attempt := 1; attempt <= s.maxAttempts; attempt++ {
		ctxAttempt, span := otel.Tracer("codeq/result_callback").Start(ctx, "codeq.webhook.task_result",
			trace.WithAttributes(
				attribute.String("codeq.command", string(cmd)),
				attribute.String("codeq.webhook.kind", "task_result"),
				attribute.String("codeq.webhook.host", host),
				attribute.Int("codeq.webhook.attempt", attempt),
			),
		)

		if s.limiter != nil && s.bucket.Enabled() {
			for {
				dec, err := s.limiter.Allow(ctxAttempt, "webhook", url, s.bucket)
				if err != nil {
					// Fail open.
					break
				}
				if dec.Allowed {
					break
				}
				metrics.RateLimitHitsTotal.WithLabelValues("webhook", "task_result").Inc()
				if sleepOrDone(ctxAttempt, dec.RetryAfter) != nil {
					span.End()
					return
				}
			}
		}

		req, _ := http.NewRequestWithContext(ctxAttempt, http.MethodPost, url, bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		s.addSignature(req, body)
		tracing.InjectHeaders(ctxAttempt, req.Header)
		resp, err := s.client.Do(req)
		if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			_ = resp.Body.Close()
			metrics.WebhookDeliveriesTotal.WithLabelValues("task_result", string(cmd), "success").Inc()
			span.End()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
			span.SetStatus(codes.Error, resp.Status)
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
		delay := s.backoffDelay(attempt)
		if sleepOrDone(ctxAttempt, delay) != nil {
			return
		}
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
