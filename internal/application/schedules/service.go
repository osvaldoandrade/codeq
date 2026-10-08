package schedules

import (
	"context"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
)

// Store is the consumer-owned persistence boundary of the schedule catalog.
type Store interface {
	// Upsert creates the schedule, confirms an identical spec, or replaces
	// the spec. Replacing a spec with the same timing keeps the firing state.
	Upsert(ctx context.Context, desired schedule.Schedule) (current schedule.Schedule, created bool, err error)
	Get(ctx context.Context, tenantID, name string) (schedule.Schedule, error)
	List(ctx context.Context, tenantID string) ([]schedule.Schedule, error)
	Delete(ctx context.Context, tenantID, name string) error
	// Due returns the schedules of every tenant whose next slot is at or
	// before now and that are not paused.
	Due(ctx context.Context, now time.Time) ([]schedule.Schedule, error)
	// Advance records a fired slot and the next one, unless the spec
	// changed since version was read (then it reports applied=false).
	Advance(ctx context.Context, scheduleID string, version int64, fired time.Time, taskID string, next time.Time) (applied bool, err error)
}

// Service coordinates validation and persistence without transport concerns.
type Service struct {
	store Store
	parse schedule.Parser
	now   func() time.Time
}

// NewService builds the schedule administration service.
func NewService(store Store, parse schedule.Parser, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, parse: parse, now: now}
}

// Upsert creates or reconciles a schedule idempotently.
func (s *Service) Upsert(ctx context.Context, tenantID, name string, spec schedule.Spec) (schedule.Schedule, bool, error) {
	desired, err := schedule.New(tenantID, name, spec, s.parse, s.now())
	if err != nil {
		return schedule.Schedule{}, false, err
	}
	return s.store.Upsert(ctx, desired)
}

// Get returns one schedule of the tenant.
func (s *Service) Get(ctx context.Context, tenantID, name string) (schedule.Schedule, error) {
	if err := schedule.ValidateIdentity(tenantID, name); err != nil {
		return schedule.Schedule{}, err
	}
	return s.store.Get(ctx, tenantID, name)
}

// List returns every schedule of the tenant, ordered by name.
func (s *Service) List(ctx context.Context, tenantID string) ([]schedule.Schedule, error) {
	if err := schedule.ValidateTenant(tenantID); err != nil {
		return nil, err
	}
	return s.store.List(ctx, tenantID)
}

// Delete removes a schedule idempotently.
func (s *Service) Delete(ctx context.Context, tenantID, name string) error {
	if err := schedule.ValidateIdentity(tenantID, name); err != nil {
		return err
	}
	return s.store.Delete(ctx, tenantID, name)
}
