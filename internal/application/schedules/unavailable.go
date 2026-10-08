package schedules

import (
	"context"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
)

type unavailableStore struct {
	reason string
}

// NewUnavailableService returns a service that fails closed for deployment
// modes where schedules cannot fire exactly once (see ADR 0006).
func NewUnavailableService(reason string) *Service {
	return NewService(&unavailableStore{reason: reason}, ParseCron, nil)
}

func (s *unavailableStore) err() error { return &schedule.UnavailableError{Reason: s.reason} }

// Upsert fails closed.
func (s *unavailableStore) Upsert(context.Context, schedule.Schedule) (schedule.Schedule, bool, error) {
	return schedule.Schedule{}, false, s.err()
}

// Get fails closed.
func (s *unavailableStore) Get(context.Context, string, string) (schedule.Schedule, error) {
	return schedule.Schedule{}, s.err()
}

// List fails closed.
func (s *unavailableStore) List(context.Context, string) ([]schedule.Schedule, error) {
	return nil, s.err()
}

// Delete fails closed.
func (s *unavailableStore) Delete(context.Context, string, string) error { return s.err() }

// Due fails closed.
func (s *unavailableStore) Due(context.Context, time.Time) ([]schedule.Schedule, error) {
	return nil, s.err()
}

// Advance fails closed.
func (s *unavailableStore) Advance(context.Context, string, int64, time.Time, string, time.Time) (bool, error) {
	return false, s.err()
}
