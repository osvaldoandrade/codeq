// Package workerclient is the Go SDK for the codeq worker streaming
// gRPC API. The high-level entry point is Client.Run, which opens a
// long-lived bidirectional stream, authenticates once, and dispatches
// claimed tasks to a user-provided Handler running on a configurable
// number of concurrent slots. Phase 2 of the throughput refactor relies
// on this client to bypass the per-call HTTP middleware tax.
package workerclient

// Task is the unit of work the handler receives. It mirrors the
// server-side domain.Task but exposes only the fields a handler
// usually needs.
type Task struct {
	ID          string
	Command     string
	Payload     []byte
	Priority    int
	Attempts    int
	MaxAttempts int
	TenantID    string
	Webhook     string
	LeaseUntil  string
}

// resultKind discriminates the four ways a handler can dispose of a task.
type resultKind int

const (
	resultCompleted resultKind = iota
	resultFailed
	resultNacked
	resultAbandoned
)

// Result is what a Handler returns. Construct one with Completed, Failed,
// Nack, or Abandon — the zero value is invalid and the client will
// reject it.
type Result struct {
	kind         resultKind
	body         map[string]any
	err          string
	delaySeconds int
	reason       string
}

// Completed marks a task done with an optional result payload. The
// payload is JSON-encoded by the server; pass nil for "no payload".
func Completed(body map[string]any) Result {
	return Result{kind: resultCompleted, body: body}
}

// Failed marks a task as permanently failed with a human-readable error.
// The scheduler will respect MaxAttempts; failures past the limit go to
// the DLQ.
func Failed(err string) Result {
	return Result{kind: resultFailed, err: err}
}

// Nack returns the task to the queue after delaySeconds. reason is
// recorded for observability. delaySeconds<0 is clamped to 0.
func Nack(delaySeconds int, reason string) Result {
	if delaySeconds < 0 {
		delaySeconds = 0
	}
	return Result{kind: resultNacked, delaySeconds: delaySeconds, reason: reason}
}

// Abandon releases the lease without nacking. The task goes straight
// back to pending and another worker can claim it immediately. Used by
// workers that are shutting down cleanly mid-task.
func Abandon() Result {
	return Result{kind: resultAbandoned}
}
