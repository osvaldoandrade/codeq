package domain

import "errors"

// QueueState names one of the per-queue indexes a task sits in. The values
// match the counters of QueueStats, so a listing and the stats of a queue
// speak the same vocabulary.
type QueueState string

const (
	// QueueStateReady lists PENDING tasks claimable now, in claim order
	// (highest priority first, FIFO within a priority).
	QueueStateReady QueueState = "ready"
	// QueueStateDelayed lists PENDING tasks waiting for their visible time,
	// earliest first.
	QueueStateDelayed QueueState = "delayed"
	// QueueStateInProgress lists IN_PROGRESS tasks held by a worker lease.
	QueueStateInProgress QueueState = "inProgress"
	// QueueStateDLQ lists FAILED tasks whose attempts ran out.
	QueueStateDLQ QueueState = "dlq"
)

// ErrInvalidQueueState rejects a listing for a state other than the
// QueueState constants. The HTTP layer answers it with 400.
var ErrInvalidQueueState = errors.New("invalid 'state' (use ready, delayed, inProgress or dlq)")

// ErrInvalidCursor rejects a listing cursor that a previous page of the same
// (tenant, command, state) listing did not return. The HTTP layer answers it
// with 400.
var ErrInvalidCursor = errors.New("invalid 'cursor'")

// ErrInvalidListLimit rejects a page size outside the bounds the scheduler
// accepts. The HTTP layer answers it with 400.
var ErrInvalidListLimit = errors.New("invalid 'limit' (use 1 to 500)")

// ParseQueueState returns the QueueState named s, or ErrInvalidQueueState.
func ParseQueueState(s string) (QueueState, error) {
	switch st := QueueState(s); st {
	case QueueStateReady, QueueStateDelayed, QueueStateInProgress, QueueStateDLQ:
		return st, nil
	default:
		return "", ErrInvalidQueueState
	}
}

// TaskPage is one page of a task listing. NextCursor is empty on the last
// page; otherwise passing it back returns the next page.
type TaskPage struct {
	Tasks      []*Task `json:"tasks"`
	NextCursor string  `json:"nextCursor,omitempty"`
}
