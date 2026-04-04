package codeq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is the CodeQ API client.
type Client struct {
	baseURL       string
	producerToken string
	workerToken   string
	adminToken    string
	httpClient    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithProducerToken sets the JWT token for producer operations.
func WithProducerToken(token string) Option {
	return func(c *Client) { c.producerToken = token }
}

// WithWorkerToken sets the JWT token for worker operations.
func WithWorkerToken(token string) Option {
	return func(c *Client) { c.workerToken = token }
}

// WithAdminToken sets the JWT token for admin operations.
func WithAdminToken(token string) Option {
	return func(c *Client) { c.adminToken = token }
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// NewClient creates a new CodeQ client.
//
// baseURL is the CodeQ server address (e.g., "http://localhost:8080").
func NewClient(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ──────────────────────────────────────────────
// Producer Operations
// ──────────────────────────────────────────────

// CreateTask creates a new task in the queue.
func (c *Client) CreateTask(ctx context.Context, opts *CreateTaskOptions) (*Task, error) {
	if c.producerToken == "" {
		return nil, fmt.Errorf("codeq: producer token is required to create tasks")
	}

	var task Task
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks", c.producerToken, opts, &task); err != nil {
		return nil, fmt.Errorf("codeq: create task: %w", err)
	}
	return &task, nil
}

// CreateTasksBatch creates multiple tasks in a single request (max 100).
func (c *Client) CreateTasksBatch(ctx context.Context, tasks []CreateTaskOptions) ([]BatchCreateResult, error) {
	if c.producerToken == "" {
		return nil, fmt.Errorf("codeq: producer token is required to create tasks")
	}

	body := struct {
		Tasks []CreateTaskOptions `json:"tasks"`
	}{Tasks: tasks}

	var results []BatchCreateResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks/batch", c.producerToken, body, &results); err != nil {
		return nil, fmt.Errorf("codeq: batch create tasks: %w", err)
	}
	return results, nil
}

// ──────────────────────────────────────────────
// Worker Operations
// ──────────────────────────────────────────────

// ClaimTask claims a task from the queue. Returns nil if no task is available.
func (c *Client) ClaimTask(ctx context.Context, opts *ClaimTaskOptions) (*Task, error) {
	if c.workerToken == "" {
		return nil, fmt.Errorf("codeq: worker token is required to claim tasks")
	}

	var task Task
	err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks/claim", c.workerToken, opts, &task)
	if err != nil {
		if apiErr, ok := err.(*Error); ok && apiErr.StatusCode == http.StatusNoContent {
			return nil, nil
		}
		return nil, fmt.Errorf("codeq: claim task: %w", err)
	}
	return &task, nil
}

// ClaimTasksBatch claims multiple tasks in a single request (max 10).
// Returns nil if no tasks are available.
func (c *Client) ClaimTasksBatch(ctx context.Context, opts *BatchClaimOptions) ([]Task, error) {
	if c.workerToken == "" {
		return nil, fmt.Errorf("codeq: worker token is required to claim tasks")
	}

	var tasks []Task
	err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks/claim/batch", c.workerToken, opts, &tasks)
	if err != nil {
		if apiErr, ok := err.(*Error); ok && apiErr.StatusCode == http.StatusNoContent {
			return nil, nil
		}
		return nil, fmt.Errorf("codeq: batch claim tasks: %w", err)
	}
	return tasks, nil
}

// SubmitResult submits a result for a completed or failed task.
func (c *Client) SubmitResult(ctx context.Context, taskID string, opts *SubmitResultOptions) (*ResultRecord, error) {
	if c.workerToken == "" {
		return nil, fmt.Errorf("codeq: worker token is required to submit results")
	}

	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/result"
	var rec ResultRecord
	if err := c.doJSON(ctx, http.MethodPost, path, c.workerToken, opts, &rec); err != nil {
		return nil, fmt.Errorf("codeq: submit result: %w", err)
	}
	return &rec, nil
}

// SubmitResultsBatch submits results for multiple tasks (max 100).
func (c *Client) SubmitResultsBatch(ctx context.Context, items []BatchSubmitItem) ([]BatchSubmitResult, error) {
	if c.workerToken == "" {
		return nil, fmt.Errorf("codeq: worker token is required to submit results")
	}

	body := struct {
		Results []BatchSubmitItem `json:"results"`
	}{Results: items}

	var results []BatchSubmitResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/tasks/batch/results", c.workerToken, body, &results); err != nil {
		return nil, fmt.Errorf("codeq: batch submit results: %w", err)
	}
	return results, nil
}

// Heartbeat extends the lease on a claimed task.
func (c *Client) Heartbeat(ctx context.Context, taskID string, extendSeconds int) error {
	if c.workerToken == "" {
		return fmt.Errorf("codeq: worker token is required for heartbeat")
	}

	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/heartbeat"
	body := struct {
		ExtendSeconds int `json:"extendSeconds"`
	}{ExtendSeconds: extendSeconds}

	if err := c.doJSON(ctx, http.MethodPost, path, c.workerToken, body, nil); err != nil {
		return fmt.Errorf("codeq: heartbeat: %w", err)
	}
	return nil
}

// Abandon abandons a claimed task, returning it to the queue.
func (c *Client) Abandon(ctx context.Context, taskID string) error {
	if c.workerToken == "" {
		return fmt.Errorf("codeq: worker token is required to abandon tasks")
	}

	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/abandon"
	if err := c.doJSON(ctx, http.MethodPost, path, c.workerToken, struct{}{}, nil); err != nil {
		return fmt.Errorf("codeq: abandon: %w", err)
	}
	return nil
}

// Nack sends a negative acknowledgment for a task with optional retry delay.
func (c *Client) Nack(ctx context.Context, taskID string, delaySeconds int, reason string) (*NackResponse, error) {
	if c.workerToken == "" {
		return nil, fmt.Errorf("codeq: worker token is required to nack tasks")
	}

	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/nack"
	body := struct {
		DelaySeconds int    `json:"delaySeconds"`
		Reason       string `json:"reason"`
	}{DelaySeconds: delaySeconds, Reason: reason}

	var resp NackResponse
	if err := c.doJSON(ctx, http.MethodPost, path, c.workerToken, body, &resp); err != nil {
		return nil, fmt.Errorf("codeq: nack: %w", err)
	}
	return &resp, nil
}

// ──────────────────────────────────────────────
// Subscription (Webhook) Operations
// ──────────────────────────────────────────────

// CreateSubscription registers a webhook subscription for push-based delivery.
func (c *Client) CreateSubscription(ctx context.Context, opts *CreateSubscriptionOptions) (*SubscriptionResponse, error) {
	if c.workerToken == "" {
		return nil, fmt.Errorf("codeq: worker token is required to create subscriptions")
	}

	var resp SubscriptionResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/workers/subscriptions", c.workerToken, opts, &resp); err != nil {
		return nil, fmt.Errorf("codeq: create subscription: %w", err)
	}
	return &resp, nil
}

// RenewSubscription extends the TTL of an existing subscription.
func (c *Client) RenewSubscription(ctx context.Context, subscriptionID string, opts *RenewSubscriptionOptions) (*SubscriptionResponse, error) {
	token := c.workerToken
	if token == "" {
		token = c.producerToken
	}
	if token == "" {
		return nil, fmt.Errorf("codeq: token is required to renew subscriptions")
	}

	path := "/v1/codeq/workers/subscriptions/" + url.PathEscape(subscriptionID) + "/heartbeat"
	body := opts
	if body == nil {
		body = &RenewSubscriptionOptions{}
	}

	var resp SubscriptionResponse
	if err := c.doJSON(ctx, http.MethodPost, path, token, body, &resp); err != nil {
		return nil, fmt.Errorf("codeq: renew subscription: %w", err)
	}
	return &resp, nil
}

// ──────────────────────────────────────────────
// Query Operations
// ──────────────────────────────────────────────

// GetTask retrieves a task by ID.
func (c *Client) GetTask(ctx context.Context, taskID string) (*Task, error) {
	token := c.workerToken
	if token == "" {
		token = c.producerToken
	}
	if token == "" {
		return nil, fmt.Errorf("codeq: token is required to get task")
	}

	path := "/v1/codeq/tasks/" + url.PathEscape(taskID)
	var task Task
	if err := c.doJSON(ctx, http.MethodGet, path, token, nil, &task); err != nil {
		return nil, fmt.Errorf("codeq: get task: %w", err)
	}
	return &task, nil
}

// GetResult retrieves the result for a task.
func (c *Client) GetResult(ctx context.Context, taskID string) (*TaskResult, error) {
	token := c.workerToken
	if token == "" {
		token = c.producerToken
	}
	if token == "" {
		return nil, fmt.Errorf("codeq: token is required to get result")
	}

	path := "/v1/codeq/tasks/" + url.PathEscape(taskID) + "/result"
	var result TaskResult
	if err := c.doJSON(ctx, http.MethodGet, path, token, nil, &result); err != nil {
		return nil, fmt.Errorf("codeq: get result: %w", err)
	}
	return &result, nil
}

// WaitForResult polls for a task result until it is available or the timeout is reached.
func (c *Client) WaitForResult(ctx context.Context, taskID string, opts *WaitForResultOptions) (*TaskResult, error) {
	timeout := 30 * time.Second
	pollInterval := 1 * time.Second

	if opts != nil {
		if opts.Timeout > 0 {
			timeout = opts.Timeout
		}
		if opts.PollInterval > 0 {
			pollInterval = opts.PollInterval
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result, err := c.GetResult(ctx, taskID)
		if err == nil {
			return result, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		wait := pollInterval
		if wait > remaining {
			wait = remaining
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}

	return nil, fmt.Errorf("codeq: timed out waiting for result of task %s after %s", taskID, timeout)
}

// ──────────────────────────────────────────────
// Admin Operations
// ──────────────────────────────────────────────

// ListQueues returns statistics for all queues.
func (c *Client) ListQueues(ctx context.Context) ([]QueueStats, error) {
	token := c.adminToken
	if token == "" {
		token = c.producerToken
	}
	if token == "" {
		return nil, fmt.Errorf("codeq: admin token is required to list queues")
	}

	var stats []QueueStats
	if err := c.doJSON(ctx, http.MethodGet, "/v1/codeq/admin/queues", token, nil, &stats); err != nil {
		return nil, fmt.Errorf("codeq: list queues: %w", err)
	}
	return stats, nil
}

// GetQueueStats returns statistics for a specific command queue.
func (c *Client) GetQueueStats(ctx context.Context, command string) (*QueueStats, error) {
	token := c.adminToken
	if token == "" {
		token = c.producerToken
	}
	if token == "" {
		return nil, fmt.Errorf("codeq: admin token is required to get queue stats")
	}

	path := "/v1/codeq/admin/queues/" + url.PathEscape(command)
	var stats QueueStats
	if err := c.doJSON(ctx, http.MethodGet, path, token, nil, &stats); err != nil {
		return nil, fmt.Errorf("codeq: get queue stats: %w", err)
	}
	return &stats, nil
}

// CleanupExpired removes expired tasks.
func (c *Client) CleanupExpired(ctx context.Context, opts *CleanupOptions) (*CleanupResult, error) {
	token := c.adminToken
	if token == "" {
		token = c.producerToken
	}
	if token == "" {
		return nil, fmt.Errorf("codeq: admin token is required for cleanup")
	}

	body := opts
	if body == nil {
		body = &CleanupOptions{}
	}

	var result CleanupResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/codeq/admin/tasks/cleanup", token, body, &result); err != nil {
		return nil, fmt.Errorf("codeq: cleanup expired: %w", err)
	}
	return &result, nil
}

// ──────────────────────────────────────────────
// Internal Helpers
// ──────────────────────────────────────────────

func (c *Client) doJSON(ctx context.Context, method, path, token string, reqBody, respBody any) error {
	u := c.baseURL + path

	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return &Error{StatusCode: resp.StatusCode, Message: "no content"}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBytes, _ := io.ReadAll(resp.Body)
		return &Error{
			StatusCode: resp.StatusCode,
			Message:    strings.TrimSpace(string(respBytes)),
		}
	}

	if respBody != nil {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}
