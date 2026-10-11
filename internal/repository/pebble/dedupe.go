package pebble

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	pebbledb "github.com/cockroachdb/pebble"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// waitingDuplicate returns the task that holds the deduplication mapping at
// mappingKey if that task still waits (PENDING, ready or delayed) and belongs
// to the same (tenant, command, key). Any other outcome returns nil and no
// error: the mapping is free or stale (its task was claimed, finished or
// reaped), and the caller overwrites it with the task it creates.
func (r *TaskRepository) waitingDuplicate(mappingKey []byte, cmd domain.Command, tenantID, key string) (*domain.Task, error) {
	id, err := r.db.Get(mappingKey)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dedupe lookup: %w", err)
	}
	body, err := r.db.Get(KeyTask(string(id)))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dedupe task lookup: %w", err)
	}
	var t domain.Task
	if err := sonic.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("dedupe task decode: %w", err)
	}
	if t.Status != domain.StatusPending || t.TenantID != tenantID || t.DeduplicationKey != key ||
		!strings.EqualFold(string(t.Command), string(cmd)) {
		return nil, nil
	}
	return &t, nil
}

// releaseDedupe deletes, inside the batch that moves t out of the waiting
// state, the deduplication mapping t holds. Only the create that wrote the
// mapping makes t its holder: a task that waits again later (a nack or
// lease-expiry retry, a dead-letter requeue) still carries its
// DeduplicationKey, but the mapping may by then name a newer waiting task of
// the same key. So the delete happens only while the mapping still names t.
// The check cannot race a create: a create that finds the mapping naming t
// while t waits returns t instead of writing, and t keeps waiting until this
// batch commits (the claim or the admin delete holds t in flight), so the
// mapping cannot move between the read and the commit.
func (r *TaskRepository) releaseDedupe(b *pebbledb.Batch, t *domain.Task) error {
	if t.DeduplicationKey == "" {
		return nil
	}
	key := KeyDedupe(t.Command, t.TenantID, t.DeduplicationKey)
	holder, err := r.db.Get(key)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dedupe release lookup: %w", err)
	}
	if string(holder) != t.ID {
		return nil
	}
	return b.Delete(key, nil)
}
