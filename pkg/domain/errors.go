package domain

import "errors"

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

// ErrDeduplicationWithIdempotency rejects a create that carries both an
// idempotency key and a deduplication key. Each key alone decides whether a
// create writes a new task, so together they would contradict each other
// (ADR 0004). The HTTP layer answers it with 400.
var ErrDeduplicationWithIdempotency = errors.New("'idempotencyKey' and 'deduplicationKey' are mutually exclusive")
