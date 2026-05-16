package pebble

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// ResultRepository backs *ResultRepository against the same Pebble DB
// that holds the task data. Atomicity for compound operations (finalize +
// inprog cleanup + lease delete + result write) comes from pebble.Batch.
type ResultRepository struct {
	db *DB
	tz *time.Location
}

// NewResultRepository creates a ResultRepository on top of an open DB.
func NewResultRepository(db *DB, tz *time.Location) *ResultRepository {
	return &ResultRepository{db: db, tz: tz}
}

func (r *ResultRepository) now() time.Time { return time.Now().In(r.tz) }

func (r *ResultRepository) GetTask(ctx context.Context, id string) (*domain.Task, error) {
	v, err := r.db.Get(KeyTask(id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("not-found")
		}
		return nil, err
	}
	var t domain.Task
	if err := sonic.Unmarshal(v, &t); err != nil {
		return nil, fmt.Errorf("unmarshal task: %w", err)
	}
	return &t, nil
}

// SaveResult writes the result record and updates the task's ResultKey
// pointer in one batch. Returns "not-found" if the task is missing — same
// contract the redis path uses so the sharded wrapper (irrelevant here
// since Pebble is single-instance) can detect orphan submits.
func (r *ResultRepository) SaveResult(ctx context.Context, rec domain.ResultRecord, _ domain.Command, _ string) error {
	taskJSON, err := r.db.Get(KeyTask(rec.TaskID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("not-found")
		}
		return err
	}
	var t domain.Task
	if err := sonic.Unmarshal(taskJSON, &t); err != nil {
		return fmt.Errorf("unmarshal task: %w", err)
	}
	t.ResultKey = "codeq:results" // mirrors the Redis sentinel for API compat
	taskUpdated, _ := sonic.Marshal(&t)
	resJSON, _ := sonic.Marshal(rec)

	b := r.db.Batch()
	defer b.Close()
	if err := b.Set(KeyResult(rec.TaskID), resJSON, nil); err != nil {
		return err
	}
	if err := b.Set(KeyTask(rec.TaskID), taskUpdated, nil); err != nil {
		return err
	}
	ttlScore := uint64(r.now().Add(taskRetention).Unix())
	if err := b.Set(KeyTTLIndex(ttlScore, rec.TaskID), nil, nil); err != nil {
		return err
	}
	return r.db.CommitBatch(b)
}

func (r *ResultRepository) GetResult(ctx context.Context, id string) (*domain.ResultRecord, error) {
	v, err := r.db.Get(KeyResult(id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("not-found")
		}
		return nil, err
	}
	var rec domain.ResultRecord
	if err := sonic.Unmarshal(v, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal result: %w", err)
	}
	return &rec, nil
}

func (r *ResultRepository) GetTaskAndResult(ctx context.Context, id string) (*domain.Task, *domain.ResultRecord, error) {
	t, terr := r.GetTask(ctx, id)
	if terr != nil {
		return nil, nil, fmt.Errorf("task not-found")
	}
	rec, rerr := r.GetResult(ctx, id)
	if rerr != nil {
		return t, nil, fmt.Errorf("result not-found")
	}
	return t, rec, nil
}

// UpdateTaskOnComplete atomically finalizes a task: status update, inprog
// removal, lease deletion. All four writes land in one batch so the claim
// path can't observe a half-completed state.
func (r *ResultRepository) UpdateTaskOnComplete(ctx context.Context, id string, cmd domain.Command, tenantID string, status domain.TaskStatus, errorMsg string) error {
	taskJSON, err := r.db.Get(KeyTask(id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("not-found")
		}
		return err
	}
	var t domain.Task
	if err := sonic.Unmarshal(taskJSON, &t); err != nil {
		return fmt.Errorf("unmarshal task: %w", err)
	}
	t.Status = status
	t.LastKnownLocation = domain.LocationNone
	t.WorkerID = ""
	t.LeaseUntil = ""
	t.UpdatedAt = r.now()
	if errorMsg != "" {
		t.Error = errorMsg
	}
	updated, _ := sonic.Marshal(&t)

	b := r.db.Batch()
	defer b.Close()
	if err := b.Set(KeyTask(id), updated, nil); err != nil {
		return err
	}
	if err := b.Delete(KeyInprog(cmd, tenantID, id), nil); err != nil {
		return err
	}
	if err := b.Delete(KeyLease(id), nil); err != nil {
		return err
	}
	ttlScore := uint64(r.now().Add(taskRetention).Unix())
	if err := b.Set(KeyTTLIndex(ttlScore, id), nil, nil); err != nil {
		return err
	}
	return r.db.CommitBatch(b)
}

func (r *ResultRepository) RemoveFromInprogAndClearLease(ctx context.Context, id string, cmd domain.Command, tenantID string) error {
	b := r.db.Batch()
	defer b.Close()
	if err := b.Delete(KeyInprog(cmd, tenantID, id), nil); err != nil {
		return err
	}
	if err := b.Delete(KeyLease(id), nil); err != nil {
		return err
	}
	return r.db.CommitBatch(b)
}

// DecodeBase64 mirrors the redis-side helper used by the result service
// when sniffing artifact bodies.
func (r *ResultRepository) DecodeBase64(str string) ([]byte, error) {
	if m := len(str) % 4; m != 0 {
		str += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(str)
}

func (r *ResultRepository) GetTasksBatch(ctx context.Context, ids []string) (map[string]*domain.Task, error) {
	out := make(map[string]*domain.Task, len(ids))
	for _, id := range ids {
		v, err := r.db.Get(KeyTask(id))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("get %s: %w", id, err)
		}
		var t domain.Task
		if err := sonic.Unmarshal(v, &t); err != nil {
			return nil, fmt.Errorf("unmarshal %s: %w", id, err)
		}
		out[id] = &t
	}
	return out, nil
}

// BatchUpdateTasksOnComplete finalizes every update in a single batch.
// Mirrors the redis TxPipeline semantics: all-or-nothing.
func (r *ResultRepository) BatchUpdateTasksOnComplete(ctx context.Context, updates []domain.TaskCompleteUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	ids := make([]string, len(updates))
	for i, u := range updates {
		ids[i] = u.ID
	}
	tasks, err := r.GetTasksBatch(ctx, ids)
	if err != nil {
		return fmt.Errorf("batch fetch tasks: %w", err)
	}

	now := r.now()
	ttlScore := uint64(now.Add(taskRetention).Unix())
	b := r.db.Batch()
	defer b.Close()
	for _, u := range updates {
		t, ok := tasks[u.ID]
		if !ok {
			return fmt.Errorf("task %s not found", u.ID)
		}
		t.Status = u.Status
		t.LastKnownLocation = domain.LocationNone
		t.WorkerID = ""
		t.LeaseUntil = ""
		t.UpdatedAt = now
		if u.ErrorMsg != "" {
			t.Error = u.ErrorMsg
		}
		updated, _ := sonic.Marshal(t)
		if err := b.Set(KeyTask(u.ID), updated, nil); err != nil {
			return err
		}
		if err := b.Delete(KeyInprog(t.Command, t.TenantID, u.ID), nil); err != nil {
			return err
		}
		if err := b.Delete(KeyLease(u.ID), nil); err != nil {
			return err
		}
		if err := b.Set(KeyTTLIndex(ttlScore, u.ID), nil, nil); err != nil {
			return err
		}
	}
	return r.db.CommitBatch(b)
}

func (r *ResultRepository) BatchRemoveFromInprogAndClearLease(ctx context.Context, deletes []domain.TaskDeleteInfo) error {
	if len(deletes) == 0 {
		return nil
	}
	b := r.db.Batch()
	defer b.Close()
	for _, d := range deletes {
		if err := b.Delete(KeyInprog(d.Command, d.TenantID, d.ID), nil); err != nil {
			return err
		}
		if err := b.Delete(KeyLease(d.ID), nil); err != nil {
			return err
		}
	}
	return r.db.CommitBatch(b)
}
