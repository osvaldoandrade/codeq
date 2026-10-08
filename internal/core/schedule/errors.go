package schedule

import "fmt"

// ValidationError reports a field-level contract violation.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Message)
}

// NotFoundError reports that a tenant-scoped schedule does not exist.
type NotFoundError struct {
	ScheduleID string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("schedule %q not found", e.ScheduleID)
}

// UnavailableError reports a deployment mode that cannot run schedules
// safely (see ADR 0006).
type UnavailableError struct {
	Reason string
}

func (e *UnavailableError) Error() string {
	return "schedules unavailable: " + e.Reason
}
