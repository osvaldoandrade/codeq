package pebble

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	pebbledb "github.com/cockroachdb/pebble"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
)

const scheduleKeyPrefix = "codeq/admin/schedules/"

type scheduleDatabase interface {
	database
	Iter(lower, upper []byte) (*pebbledb.Iterator, error)
}

// ScheduleStore keeps the recurring schedule catalog in Pebble. Writes are
// serialized on one node and, in Raft mode, rejected on followers and
// replicated as one Pebble batch, exactly like the topic catalog.
type ScheduleStore struct {
	db scheduleDatabase
	mu sync.Mutex
}

// NewScheduleStore builds a tenant-scoped schedule catalog over Pebble.
func NewScheduleStore(db *pebblerepo.DB) *ScheduleStore {
	return &ScheduleStore{db: db}
}

// Upsert creates the schedule, confirms an identical spec, or replaces the
// spec (bumping Version). A replacement with the same timing keeps the next
// slot; a new timing takes the slot computed for the desired schedule. The
// last fired slot and task are kept in both cases.
func (s *ScheduleStore) Upsert(ctx context.Context, desired schedule.Schedule) (schedule.Schedule, bool, error) {
	if err := schedule.ValidateIdentity(desired.TenantID, desired.Name); err != nil {
		return schedule.Schedule{}, false, err
	}
	if desired.ScheduleID != schedule.PhysicalID(desired.TenantID, desired.Name) {
		return schedule.Schedule{}, false, errors.New("schedule identity does not match physical id")
	}
	unlock, err := s.lockForWrite(ctx)
	if err != nil {
		return schedule.Schedule{}, false, err
	}
	defer unlock()

	current, exists, err := s.read(desired.ScheduleID)
	if err != nil {
		return schedule.Schedule{}, false, err
	}
	if exists && schedule.SameSpec(current.Spec, desired.Spec) {
		return current, false, nil
	}
	desired.Version = 1
	if exists {
		desired.Version = current.Version + 1
		desired.CreatedAt = current.CreatedAt
		desired.LastRunAt, desired.LastTaskID = current.LastRunAt, current.LastTaskID
		if schedule.SameTiming(current.Spec, desired.Spec) {
			desired.NextRunAt = current.NextRunAt
		}
	}
	if err := s.write(desired); err != nil {
		return schedule.Schedule{}, false, err
	}
	return desired, !exists, nil
}

// Get returns one schedule of the tenant.
func (s *ScheduleStore) Get(ctx context.Context, tenantID, name string) (schedule.Schedule, error) {
	if err := ctx.Err(); err != nil {
		return schedule.Schedule{}, err
	}
	id := schedule.PhysicalID(tenantID, name)
	current, exists, err := s.read(id)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if !exists {
		return schedule.Schedule{}, &schedule.NotFoundError{ScheduleID: id}
	}
	return current, nil
}

// List returns every schedule of the tenant, ordered by name. Tenants and
// names contain no '.', so the "<tenant>." prefix matches only that tenant.
func (s *ScheduleStore) List(ctx context.Context, tenantID string) ([]schedule.Schedule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.scan([]byte(scheduleKeyPrefix+tenantID+"."), func(schedule.Schedule) bool { return true })
}

// Delete removes a schedule idempotently. Followers are rejected even when
// the key is absent so they never acknowledge a local write.
func (s *ScheduleStore) Delete(ctx context.Context, tenantID, name string) error {
	unlock, err := s.lockForWrite(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	id := schedule.PhysicalID(tenantID, name)
	if err := s.db.Delete(scheduleKey(id)); err != nil {
		return fmt.Errorf("delete schedule %q: %w", id, err)
	}
	return nil
}

// Due returns the unpaused schedules of every tenant whose next slot is at
// or before now, earliest first.
func (s *ScheduleStore) Due(ctx context.Context, now time.Time) ([]schedule.Schedule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	due, err := s.scan([]byte(scheduleKeyPrefix), func(sc schedule.Schedule) bool {
		return !sc.Spec.Paused && !sc.NextRunAt.IsZero() && !sc.NextRunAt.After(now)
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].NextRunAt.Before(due[j].NextRunAt) })
	return due, nil
}

// Advance records that the slot fired and moves to the next one. It is a
// compare-and-set on (version, next slot): if the spec changed, the schedule
// was paused or deleted, or another fire already recorded this slot, nothing
// is written and applied is false.
func (s *ScheduleStore) Advance(ctx context.Context, scheduleID string, version int64, fired time.Time, taskID string, next time.Time) (bool, error) {
	unlock, err := s.lockForWrite(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	current, exists, err := s.read(scheduleID)
	if err != nil {
		return false, err
	}
	if !exists || current.Version != version || current.Spec.Paused || !current.NextRunAt.Equal(fired) {
		return false, nil
	}
	firedUTC := fired.UTC()
	current.LastRunAt, current.LastTaskID, current.NextRunAt = &firedUTC, taskID, next.UTC()
	if err := s.write(current); err != nil {
		return false, err
	}
	return true, nil
}

// lockForWrite checks write leadership, takes the store mutex and checks
// again, so a node that lost leadership while waiting never writes.
func (s *ScheduleStore) lockForWrite(ctx context.Context) (func(), error) {
	if err := s.checkWrite(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if err := s.checkWrite(ctx); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	return s.mu.Unlock, nil
}

func (s *ScheduleStore) checkWrite(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.RequireWriteLeader()
}

func (s *ScheduleStore) write(sc schedule.Schedule) error {
	payload, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("marshal schedule %q: %w", sc.ScheduleID, err)
	}
	if err := s.db.Set(scheduleKey(sc.ScheduleID), payload); err != nil {
		return fmt.Errorf("persist schedule %q: %w", sc.ScheduleID, err)
	}
	return nil
}

func (s *ScheduleStore) read(id string) (schedule.Schedule, bool, error) {
	payload, err := s.db.Get(scheduleKey(id))
	if errors.Is(err, pebblerepo.ErrNotFound) {
		return schedule.Schedule{}, false, nil
	}
	if err != nil {
		return schedule.Schedule{}, false, fmt.Errorf("read schedule %q: %w", id, err)
	}
	sc, err := decodeSchedule(payload)
	if err != nil {
		return schedule.Schedule{}, false, fmt.Errorf("decode schedule %q: %w", id, err)
	}
	if sc.ScheduleID != id {
		return schedule.Schedule{}, false, fmt.Errorf("decode schedule %q: stored identity does not match key", id)
	}
	return sc, true, nil
}

func (s *ScheduleStore) scan(prefix []byte, keep func(schedule.Schedule) bool) ([]schedule.Schedule, error) {
	it, err := s.db.Iter(prefix, prefixEnd(prefix))
	if err != nil {
		return nil, err
	}
	defer it.Close()
	out := []schedule.Schedule{}
	for valid := it.First(); valid; valid = it.Next() {
		sc, err := decodeSchedule(it.Value())
		if err != nil {
			return nil, fmt.Errorf("decode schedule %q: %w", it.Key(), err)
		}
		if keep(sc) {
			out = append(out, sc)
		}
	}
	return out, it.Error()
}

func decodeSchedule(payload []byte) (schedule.Schedule, error) {
	var sc schedule.Schedule
	err := json.Unmarshal(payload, &sc)
	return sc, err
}

func scheduleKey(id string) []byte {
	return []byte(scheduleKeyPrefix + id)
}

// prefixEnd is the smallest key greater than every key starting with p;
// p always ends in an ASCII byte here.
func prefixEnd(p []byte) []byte {
	end := append([]byte(nil), p...)
	end[len(end)-1]++
	return end
}
