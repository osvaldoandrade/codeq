package queuetopic

import "fmt"

// ValidationError reports a field-level contract violation.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Message)
}

// NotFoundError reports that a tenant-scoped topic does not exist.
type NotFoundError struct {
	TopicID string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("queue topic %q not found", e.TopicID)
}

// ConflictError reports that a concurrent write could not be resolved safely.
type ConflictError struct {
	TopicID string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("queue topic %q changed concurrently", e.TopicID)
}

// UnavailableError reports a storage mode that cannot safely persist topics.
type UnavailableError struct {
	Reason string
}

func (e *UnavailableError) Error() string {
	return "queue topic administration unavailable: " + e.Reason
}
