package domain

import "errors"

// ErrTaskNotInDLQ rejects a requeue of a task that is not dead-lettered:
// its status is not FAILED or it has no dead-letter queue entry (a task
// failed through a result submit, for example). The HTTP layer answers it
// with 409 and this message as the error code.
var ErrTaskNotInDLQ = errors.New("task_not_in_dlq")

// ErrTaskInProgress rejects the deletion of a task that a worker holds (or
// is claiming) under a lease. The HTTP layer answers it with 409 and this
// message as the error code.
var ErrTaskInProgress = errors.New("task_in_progress")

// ErrInvalidRequeueLimit rejects a bulk requeue limit outside the bounds the
// scheduler accepts. The HTTP layer answers it with 400.
var ErrInvalidRequeueLimit = errors.New("invalid 'limit' (use 1 to 1000)")

// DLQRequeue is the outcome of one bulk requeue of a dead-letter queue.
// Requeued counts the tasks moved back to the ready queue by this call.
// Remaining reports that the dead-letter queue still holds entries, so a
// caller drains it by repeating the call until Remaining is false.
type DLQRequeue struct {
	Requeued  int  `json:"requeued"`
	Remaining bool `json:"remaining"`
}
