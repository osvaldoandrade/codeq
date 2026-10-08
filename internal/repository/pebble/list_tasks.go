package pebble

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"

	"github.com/bytedance/sonic"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// keyRange is a [lower, upper) span of one queue index.
type keyRange struct {
	lower, upper []byte
}

func (k keyRange) contains(key []byte) bool {
	return bytes.Compare(key, k.lower) >= 0 && bytes.Compare(key, k.upper) < 0
}

// stateRanges returns the index spans of a (cmd, tenant) queue state in
// listing order. Ready walks the priority buckets from highest to lowest, so
// a listing follows claim order; every other state is one span.
func stateRanges(cmd domain.Command, tenantID string, state domain.QueueState) ([]keyRange, error) {
	var lower, upper []byte
	switch state {
	case domain.QueueStateReady:
		ranges := make([]keyRange, 0, maxPriority-minPriority+1)
		for p := maxPriority; p >= minPriority; p-- {
			lo, hi := PrefixPendingPrio(cmd, tenantID, p)
			ranges = append(ranges, keyRange{lo, hi})
		}
		return ranges, nil
	case domain.QueueStateDelayed:
		lower, upper = PrefixDelayed(cmd, tenantID)
	case domain.QueueStateInProgress:
		lower, upper = PrefixInprog(cmd, tenantID)
	case domain.QueueStateDLQ:
		lower, upper = PrefixDLQ(cmd, tenantID)
	default:
		return nil, domain.ErrInvalidQueueState
	}
	return []keyRange{{lower, upper}}, nil
}

// stateStatus is the status a task must have to be listed under state. An
// index entry whose task has another status is stale (a duplicate hint, or a
// transition in flight) and is skipped.
func stateStatus(state domain.QueueState) domain.TaskStatus {
	switch state {
	case domain.QueueStateReady, domain.QueueStateDelayed:
		return domain.StatusPending
	case domain.QueueStateInProgress:
		return domain.StatusInProgress
	case domain.QueueStateDLQ:
		return domain.StatusFailed
	default:
		return ""
	}
}

// ListTasks returns up to limit tasks of one (cmd, tenant) queue state, in
// listing order, resuming after cursor. The cursor is the last index key a
// page consumed; a cursor outside the listed queue state fails with
// domain.ErrInvalidCursor, so it can never reach another tenant's keys.
func (r *TaskRepository) ListTasks(ctx context.Context, cmd domain.Command, tenantID string, state domain.QueueState, limit int, cursor string) (*domain.TaskPage, error) {
	ranges, err := stateRanges(cmd, tenantID, state)
	if err != nil {
		return nil, err
	}
	start, after, err := resumePoint(ranges, cursor)
	if err != nil {
		return nil, err
	}
	page := &domain.TaskPage{Tasks: []*domain.Task{}}
	want := stateStatus(state)
	for i := start; i < len(ranges) && len(page.Tasks) < limit; i++ {
		last, more, err := r.scanRange(ranges[i], after, want, limit, page)
		if err != nil {
			return nil, err
		}
		after = nil
		if !more && len(page.Tasks) >= limit {
			if more, err = r.anyEntry(ranges[i+1:]); err != nil {
				return nil, err
			}
		}
		if more {
			page.NextCursor = base64.RawURLEncoding.EncodeToString(last)
		}
	}
	return page, nil
}

// resumePoint decodes a cursor into the span it falls in and the key to
// resume after. An empty cursor starts at the first span.
func resumePoint(ranges []keyRange, cursor string) (int, []byte, error) {
	if cursor == "" {
		return 0, nil, nil
	}
	key, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(key) == 0 {
		return 0, nil, domain.ErrInvalidCursor
	}
	for i, rg := range ranges {
		if rg.contains(key) {
			return i, key, nil
		}
	}
	return 0, nil, domain.ErrInvalidCursor
}

// scanRange appends to page the tasks of rg after the key `after` (from the
// start when nil) until page holds limit tasks. It returns the last index key
// it consumed and whether rg still holds entries past it.
func (r *TaskRepository) scanRange(rg keyRange, after []byte, want domain.TaskStatus, limit int, page *domain.TaskPage) ([]byte, bool, error) {
	it, err := r.db.Iter(rg.lower, rg.upper)
	if err != nil {
		return nil, false, err
	}
	defer it.Close()

	valid := it.First()
	if after != nil {
		valid = it.SeekGE(after)
		if valid && bytes.Equal(it.Key(), after) {
			valid = it.Next()
		}
	}
	var last []byte
	for ; valid; valid = it.Next() {
		if len(page.Tasks) >= limit {
			return last, true, nil
		}
		last = append(last[:0], it.Key()...)
		task, err := r.indexedTask(last, want)
		if err != nil {
			return nil, false, err
		}
		if task != nil {
			page.Tasks = append(page.Tasks, task)
		}
	}
	return last, false, it.Error()
}

// indexedTask loads the task an index key points at (its id is the last
// segment). It returns nil without error when the body is gone, does not
// decode, or no longer has the listed status: such entries are stale.
func (r *TaskRepository) indexedTask(key []byte, want domain.TaskStatus) (*domain.Task, error) {
	id := key[bytes.LastIndexByte(key, '/')+1:]
	body, err := r.db.Get(KeyTask(string(id)))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t domain.Task
	decoded := sonic.Unmarshal(body, &t) == nil
	if !decoded || t.Status != want {
		return nil, nil
	}
	return &t, nil
}

// anyEntry reports whether any of the spans holds at least one index entry.
func (r *TaskRepository) anyEntry(ranges []keyRange) (bool, error) {
	for _, rg := range ranges {
		it, err := r.db.Iter(rg.lower, rg.upper)
		if err != nil {
			return false, err
		}
		found := it.First()
		if err := it.Close(); err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}
