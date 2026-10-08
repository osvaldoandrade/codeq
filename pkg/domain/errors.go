package domain

import (
	"errors"
	"strings"
)

// LeaderHint is satisfied by errors that carry a hint pointing at the
// raft group's current leader (HTTP base URL, e.g. "http://node-2:8080").
// The HTTP layer uses errors.As against this interface to detect a
// "not leader" error and respond with a 307 redirect to the leader.
//
// The interface lives in pkg/domain so the storage layer (where the
// error is constructed) and the HTTP layer (where it's interpreted)
// share a vocabulary without introducing a circular dependency.
type LeaderHint interface {
	error
	LeaderHTTPAddr() string
}

// ErrIdempotencyConflict is returned by every TaskRepository backend when an
// idempotency key already maps to a task of a different tenant. The caller
// receives neither the task nor its ID (platform ADR-0022 C1.2: no
// cross-tenant existence oracle with data). Its message is the stable wire
// code the HTTP layer answers with 409.
var ErrIdempotencyConflict = errors.New("idempotency_conflict")

// ErrTaskIDConflict is returned when a create names a task ID that another
// tenant's task already uses. Like ErrIdempotencyConflict, the caller gets
// neither the task nor its data; the message is the wire code the HTTP layer
// answers with 409.
var ErrTaskIDConflict = errors.New("task_id_conflict")

// ErrTaskIDWithIdempotency rejects a create that carries both a task ID and
// an idempotency key: a named task is already idempotent by its ID. The HTTP
// layer answers it with 400.
var ErrTaskIDWithIdempotency = errors.New("'taskId' and 'idempotencyKey' are mutually exclusive")

// ErrInvalidTaskID rejects a client-chosen task ID outside the allowed shape
// (see ValidTaskID). The HTTP layer answers it with 400.
var ErrInvalidTaskID = errors.New("invalid 'taskId' (1-200 characters from [A-Za-z0-9._:@+-], starting with a letter or digit)")

// ValidTaskID reports whether id may name a task: 1 to 200 characters from
// [A-Za-z0-9._:@+-], starting with a letter or digit. '/' is excluded
// because storage keys end in "/<task id>", and NUL because it separates
// binding namespaces.
func ValidTaskID(id string) bool {
	if id == "" || len(id) > maxTaskIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !taskIDByte(id[i], i == 0) {
			return false
		}
	}
	return true
}

// taskIDByte reports whether c may appear in a task ID; punctuation is
// allowed anywhere but first.
func taskIDByte(c byte, first bool) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	return !first && strings.IndexByte("._:@+-", c) >= 0
}

const maxTaskIDLength = 200
