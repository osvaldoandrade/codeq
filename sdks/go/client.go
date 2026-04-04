// Package codeq provides a Go client SDK for the codeQ task-queue API.
//
// The Client supports producer operations (creating tasks), worker operations
// (claiming, completing, and failing tasks), subscription management, and
// administrative queries—all over the codeQ REST API with automatic retry and
// configurable timeouts.
//
// Basic usage:
//
//	client := codeq.NewClient("http://localhost:8080",
//		codeq.WithProducerToken("tok-producer"),
//		codeq.WithWorkerToken("tok-worker"),
//	)
//
//	task, err := client.CreateTask(ctx, codeq.CreateTaskOptions{
//		Command: "PROCESS_IMAGE",
//		Payload: map[string]any{"url": "https://example.com/img.png"},
//	})
package codeq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientConfig holds the resolved configuration for a [Client].
type ClientConfig struct {
	// BaseURL is the root URL of the codeQ server (e.g. "http://localhost:8080").
	BaseURL string
	// ProducerToken is the JWT used for task-creation endpoints.
	ProducerToken string
	// WorkerToken is the JWT used for task-claiming and result endpoints.
	WorkerToken string
	// AdminToken is the JWT used for administrative endpoints.
	AdminToken string
	// HTTPClient is the underlying HTTP client. When nil, a client with a 30 s
	// timeout is created automatically.
	HTTPClient *http.Client
	// MaxRetries is the number of times a transient failure (5xx, network error)
	// is retried. Default: 3.
	MaxRetries int
	// RetryBaseDelay is the base delay for exponential backoff between retries.
	// Default: 500 ms.
	RetryBaseDelay time.Duration
}

// Option is a functional option for configuring a [Client].
type Option func(*ClientConfig)

// WithProducerToken sets the JWT token used for producer operations.
func WithProducerToken(token string) Option {
	return func(c *ClientConfig) { c.ProducerToken = token }
}

// WithWorkerToken sets the JWT token used for worker operations.
func WithWorkerToken(token string) Option {
	return func(c *ClientConfig) { c.WorkerToken = token }
}

// WithAdminToken sets the JWT token used for admin operations.
func WithAdminToken(token string) Option {
	return func(c *ClientConfig) { c.AdminToken = token }
}

// WithHTTPClient overrides the default HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *ClientConfig) { c.HTTPClient = hc }
}

// WithMaxRetries sets the maximum number of retry attempts for transient
// errors. Set to 0 to disable retries.
func WithMaxRetries(n int) Option {
	return func(c *ClientConfig) { c.MaxRetries = n }
}

// WithRetryBaseDelay sets the base delay for exponential back-off between
// retries.
func WithRetryBaseDelay(d time.Duration) Option {
	return func(c *ClientConfig) { c.RetryBaseDelay = d }
}

// Client is the primary interface to the codeQ API.
type Client struct {
	cfg ClientConfig
}

// NewClient creates a new codeQ API client for the given base URL. Configure
// authentication tokens and other options with [Option] values.
func NewClient(baseURL string, opts ...Option) *Client {
	cfg := ClientConfig{
		BaseURL:        strings.TrimRight(baseURL, "/"),
		MaxRetries:     3,
		RetryBaseDelay: 500 * time.Millisecond,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{cfg: cfg}
}

// ---------- Producer operations ----------

// CreateTask creates a new task in the queue.
func (c *Client) CreateTask(ctx context.Context, opts CreateTaskOptions) (*Task, error) {
	var task Task
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks", c.cfg.ProducerToken, opts, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// CreateTasksBatch creates multiple tasks in a single request (up to 100).
func (c *Client) CreateTasksBatch(ctx context.Context, tasks []CreateTaskOptions) ([]BatchTaskResult, error) {
	var results []BatchTaskResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks/batch", c.cfg.ProducerToken, tasks, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// ---------- Worker operations ----------

// ClaimTask attempts to claim a task from the queue. Returns nil (without
// error) when no task is available.
func (c *Client) ClaimTask(ctx context.Context, opts ClaimTaskOptions) (*Task, error) {
	var task Task
	err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks/claim", c.cfg.WorkerToken, opts, &task)
	if err != nil {
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == http.StatusNoContent {
			return nil, nil
		}
		return nil, err
	}
	return &task, nil
}

// ClaimTasksBatch claims multiple tasks in a single request (up to 10).
func (c *Client) ClaimTasksBatch(ctx context.Context, opts ClaimTaskOptions) ([]Task, error) {
	var tasks []Task
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks/claim/batch", c.cfg.WorkerToken, opts, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// SubmitResult submits the result of a processed task.
func (c *Client) SubmitResult(ctx context.Context, taskID string, opts SubmitResultOptions) (*ResultRecord, error) {
	var rec ResultRecord
	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/result"
	if err := c.doJSON(ctx, http.MethodPost, path, c.cfg.WorkerToken, opts, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// SubmitResultsBatch submits results for multiple tasks in a single request
// (up to 100).
func (c *Client) SubmitResultsBatch(ctx context.Context, submissions []BatchResultSubmission) ([]BatchTaskResult, error) {
	var results []BatchTaskResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks/batch/results", c.cfg.WorkerToken, submissions, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// Heartbeat extends the lease of a claimed task, preventing it from being
// reclaimed.
func (c *Client) Heartbeat(ctx context.Context, taskID string, extendSeconds int) error {
	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/heartbeat"
	body := map[string]int{"extendSeconds": extendSeconds}
	return c.doJSON(ctx, http.MethodPost, path, c.cfg.WorkerToken, body, nil)
}

// Abandon returns a claimed task to the queue so another worker can claim it.
func (c *Client) Abandon(ctx context.Context, taskID string) error {
	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/abandon"
	return c.doJSON(ctx, http.MethodPost, path, c.cfg.WorkerToken, nil, nil)
}

// Nack negatively acknowledges a task, causing it to be retried after a delay.
func (c *Client) Nack(ctx context.Context, taskID string, delaySeconds int, reason string) (*NackResponse, error) {
	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/nack"
	body := map[string]any{"delaySeconds": delaySeconds, "reason": reason}
	var resp NackResponse
	if err := c.doJSON(ctx, http.MethodPost, path, c.cfg.WorkerToken, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------- Subscription operations ----------

// CreateSubscription registers a webhook subscription for task events.
func (c *Client) CreateSubscription(ctx context.Context, opts CreateSubscriptionOptions) (*SubscriptionResponse, error) {
	var resp SubscriptionResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/workers/subscriptions", c.cfg.WorkerToken, opts, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RenewSubscription extends the lifetime of an existing webhook subscription.
func (c *Client) RenewSubscription(ctx context.Context, subscriptionID string, opts *RenewSubscriptionOptions) (*SubscriptionResponse, error) {
	path := "/v1/codeq/workers/subscriptions/" + url.PathEscape(subscriptionID) + "/heartbeat"
	var resp SubscriptionResponse
	if err := c.doJSON(ctx, http.MethodPost, path, c.cfg.WorkerToken, opts, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---------- Query operations ----------

// GetTask retrieves the current state of a task by ID.
func (c *Client) GetTask(ctx context.Context, taskID string) (*Task, error) {
	var task Task
	path := "/v1/codeq/tasks/" + url.PathEscape(taskID)
	if err := c.doJSON(ctx, http.MethodGet, path, c.tokenForRead(), nil, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// GetResult retrieves the result of a completed task.
func (c *Client) GetResult(ctx context.Context, taskID string) (*TaskResult, error) {
	var result TaskResult
	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/result"
	if err := c.doJSON(ctx, http.MethodGet, path, c.tokenForRead(), nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// WaitForResult polls for a task result until it arrives or the timeout
// expires. Default timeout is 30 s and default poll interval is 1 s.
func (c *Client) WaitForResult(ctx context.Context, taskID string, opts *WaitForResultOptions) (*TaskResult, error) {
	timeout := 30 * time.Second
	interval := 1 * time.Second
	if opts != nil {
		if opts.Timeout > 0 {
			timeout = opts.Timeout
		}
		if opts.PollInterval > 0 {
			interval = opts.PollInterval
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// Check context before issuing a request.
		select {
		case <-ctx.Done():
			return nil, &TimeoutError{Message: fmt.Sprintf("timed out waiting for result of task %s", taskID)}
		default:
		}

		result, err := c.GetResult(ctx, taskID)
		if err == nil {
			return result, nil
		}

		// Context-related errors (deadline exceeded, cancelled) → timeout.
		if ctx.Err() != nil {
			return nil, &TimeoutError{Message: fmt.Sprintf("timed out waiting for result of task %s", taskID)}
		}

		// If it's a 404, the result is not yet available—keep polling.
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == http.StatusNotFound {
			select {
			case <-ctx.Done():
				return nil, &TimeoutError{Message: fmt.Sprintf("timed out waiting for result of task %s", taskID)}
			case <-ticker.C:
				continue
			}
		}
		return nil, err
	}
}

// ---------- Admin operations ----------

// ListQueues returns statistics for all known queues.
func (c *Client) ListQueues(ctx context.Context) ([]QueueStats, error) {
	var stats []QueueStats
	if err := c.doJSON(ctx, http.MethodGet, "/v1/codeq/admin/queues", c.cfg.AdminToken, nil, &stats); err != nil {
		return nil, err
	}
	return stats, nil
}

// GetQueueStats returns statistics for a single command queue.
func (c *Client) GetQueueStats(ctx context.Context, command string) (*QueueStats, error) {
	var stats QueueStats
	path := "/v1/codeq/admin/queues/" + url.PathEscape(command)
	if err := c.doJSON(ctx, http.MethodGet, path, c.cfg.AdminToken, nil, &stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

// CleanupExpired removes expired tasks from the system.
func (c *Client) CleanupExpired(ctx context.Context, opts *CleanupOptions) (*CleanupResult, error) {
	var result CleanupResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/admin/tasks/cleanup", c.cfg.AdminToken, opts, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ---------- internal helpers ----------

// tokenForRead returns the first available token for read-only operations,
// preferring producer > worker > admin.
func (c *Client) tokenForRead() string {
	if c.cfg.ProducerToken != "" {
		return c.cfg.ProducerToken
	}
	if c.cfg.WorkerToken != "" {
		return c.cfg.WorkerToken
	}
	return c.cfg.AdminToken
}

// doJSON performs an HTTP request, encoding the body as JSON and decoding the
// response into dest. It handles authentication, error classification, and
// retries.
func (c *Client) doJSON(ctx context.Context, method, path, token string, body any, dest any) error {
	var lastErr error

	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := c.cfg.RetryBaseDelay * time.Duration(math.Pow(2, float64(attempt-1)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := c.doOnce(ctx, method, path, token, body, dest)
		if err == nil {
			return nil
		}

		// Retry only on server errors (5xx) or context-unrelated network
		// errors. Client errors (4xx) are not retried.
		if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode < 500 {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (c *Client) doOnce(ctx context.Context, method, path, token string, body any, dest any) error {
	fullURL := c.cfg.BaseURL + path

	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return &Error{Message: "failed to encode request body", Cause: err}
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return &Error{Message: "failed to create request", Cause: err}
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return &Error{Message: "request failed", Cause: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Error{Message: "failed to read response body", Cause: err}
	}

	if resp.StatusCode >= 400 {
		msg := http.StatusText(resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return &AuthError{Message: msg}
		}
		return &APIError{
			StatusCode:   resp.StatusCode,
			ResponseBody: string(respBody),
			Message:      msg,
		}
	}

	// Some endpoints return 204 No Content—signal via APIError so callers
	// (e.g. ClaimTask) can handle it gracefully.
	if resp.StatusCode == http.StatusNoContent || len(respBody) == 0 {
		if dest != nil {
			return &APIError{StatusCode: http.StatusNoContent, Message: "no content"}
		}
		return nil
	}

	if dest != nil {
		if err := json.Unmarshal(respBody, dest); err != nil {
			return &Error{Message: "failed to decode response", Cause: err}
		}
	}
	return nil
}
