package domain

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
