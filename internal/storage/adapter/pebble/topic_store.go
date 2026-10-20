// Package pebble implements QueueTopic persistence on the embedded Pebble DB.
package pebble

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
)

const topicKeyPrefix = "codeq/admin/topics/"

type database interface {
	RequireWriteLeader() error
	Get([]byte) ([]byte, error)
	Set([]byte, []byte) error
	Delete([]byte) error
}

// TopicStore serializes catalog read-modify-write operations on one node. In
// raft mode the DB rejects followers and replicates the resulting Pebble batch.
type TopicStore struct {
	db       database
	mu       sync.Mutex
	policyMu sync.RWMutex
	custody  func(context.Context, string, string) (bool, error)
	durable  func() bool
}

// NewTopicStore builds a tenant-scoped topic catalog over Pebble.
func NewTopicStore(db *pebblerepo.DB, otherShards ...*pebblerepo.DB) *TopicStore {
	dbs := append([]*pebblerepo.DB{db}, otherShards...)
	return &TopicStore{db: db, durable: func() bool {
		for _, shard := range dbs {
			if !shard.DurableWrites() {
				return false
			}
		}
		return true
	}, custody: func(ctx context.Context, tenant, name string) (bool, error) {
		for _, shard := range dbs {
			active, err := shard.HasAbsoluteTopicCustody(ctx, tenant, name)
			if err != nil || active {
				return active, err
			}
		}
		return false, nil
	}}
}

// AcquireAbsolutePolicyRead keeps a local acceptance's final policy check and
// commit on one side of a disable/delete decision across every local shard.
func (s *TopicStore) AcquireAbsolutePolicyRead() func() {
	s.policyMu.RLock()
	return s.policyMu.RUnlock
}

// Upsert atomically creates, updates, or confirms an identical topic policy.
func (s *TopicStore) Upsert(ctx context.Context, desired queuetopic.Topic) (queuetopic.Topic, bool, bool, error) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if desired.Policy.AbsoluteExpiry && (s.durable == nil || !s.durable()) {
		return queuetopic.Topic{}, false, false, &queuetopic.ValidationError{Field: "absoluteExpiry", Message: "requires fsyncOnCommit=true on every storage member"}
	}
	if err := validateDesired(desired); err != nil {
		return queuetopic.Topic{}, false, false, err
	}
	if err := s.checkWrite(ctx); err != nil {
		return queuetopic.Topic{}, false, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWrite(ctx); err != nil {
		return queuetopic.Topic{}, false, false, err
	}

	current, exists, err := s.read(desired.TopicID)
	if err != nil {
		return queuetopic.Topic{}, false, false, err
	}
	if err = s.preserveAbsolutePolicy(ctx, current, &desired, exists); err != nil {
		return queuetopic.Topic{}, false, false, err
	}
	if exists && queuetopic.SamePolicy(current.Policy, desired.Policy) {
		return current, false, false, nil
	}

	created := !exists
	changed := exists
	if exists {
		desired.CreatedAt = current.CreatedAt
		desired.Version = current.Version + 1
	} else {
		desired.Version = 1
	}
	payload, err := json.Marshal(desired)
	if err != nil {
		return queuetopic.Topic{}, false, false, fmt.Errorf("marshal queue topic %q: %w", desired.TopicID, err)
	}
	if err := s.db.Set(topicKey(desired.TopicID), payload); err != nil {
		return queuetopic.Topic{}, false, false, fmt.Errorf("persist queue topic %q: %w", desired.TopicID, err)
	}
	return desired, created, changed, nil
}

func (s *TopicStore) preserveAbsolutePolicy(ctx context.Context, current queuetopic.Topic, desired *queuetopic.Topic, exists bool) error {
	if !exists || !current.Policy.AbsoluteExpiry {
		return nil
	}
	if !desired.Policy.AbsoluteExpiry {
		return s.requireEmptyAbsoluteCustody(ctx, current)
	}
	floor := current.Policy.VisibilityFloor
	if desired.Policy.VisibilityFloor != nil && (floor == nil || desired.Policy.VisibilityFloor.After(*floor)) {
		floor = desired.Policy.VisibilityFloor
	}
	if current.Policy.RetentionSeconds != desired.Policy.RetentionSeconds {
		retention := min(current.Policy.RetentionSeconds, desired.Policy.RetentionSeconds)
		cutoff := desired.UpdatedAt.Add(-time.Duration(retention) * time.Second)
		if floor == nil || cutoff.After(*floor) {
			floor = &cutoff
		}
	}
	desired.Policy.VisibilityFloor = floor
	return nil
}

// Get returns one topic within the authenticated tenant.
func (s *TopicStore) Get(ctx context.Context, tenantID, topicName string) (queuetopic.Topic, error) {
	if err := ctx.Err(); err != nil {
		return queuetopic.Topic{}, err
	}
	topicID := queuetopic.PhysicalID(tenantID, topicName)
	topic, exists, err := s.read(topicID)
	if err != nil {
		return queuetopic.Topic{}, err
	}
	if !exists {
		return queuetopic.Topic{}, &queuetopic.NotFoundError{TopicID: topicID}
	}
	return topic, nil
}

// Delete removes a topic catalog entry idempotently. Followers are rejected
// even when the key is already absent so they never acknowledge local writes.
func (s *TopicStore) Delete(ctx context.Context, tenantID, topicName string) error {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if err := s.checkWrite(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWrite(ctx); err != nil {
		return err
	}
	topicID := queuetopic.PhysicalID(tenantID, topicName)
	// Absolute policies retain their monotonic cutoff. Removing it while rows
	// await cleanup would let a recreated catalog entry expose those rows again.
	if current, exists, err := s.read(topicID); err != nil {
		return err
	} else if exists && current.Policy.AbsoluteExpiry {
		if err := s.requireEmptyAbsoluteCustody(ctx, current); err != nil {
			return err
		}
	}
	if err := s.db.Delete(topicKey(topicID)); err != nil {
		return fmt.Errorf("delete queue topic %q: %w", topicID, err)
	}
	return nil
}

func (s *TopicStore) requireEmptyAbsoluteCustody(ctx context.Context, topic queuetopic.Topic) error {
	if s.custody != nil {
		active, err := s.custody(ctx, topic.TenantID, topic.TopicName)
		if err != nil {
			return err
		}
		if active {
			return &queuetopic.ConflictError{TopicID: topic.TopicID}
		}
		return nil
	}
	inspector, ok := s.db.(interface {
		HasAbsoluteTopicCustody(context.Context, string, string) (bool, error)
	})
	if !ok {
		return &queuetopic.ConflictError{TopicID: topic.TopicID}
	}
	active, err := inspector.HasAbsoluteTopicCustody(ctx, topic.TenantID, topic.TopicName)
	if err != nil {
		return err
	}
	if active {
		return &queuetopic.ConflictError{TopicID: topic.TopicID}
	}
	return nil
}

func (s *TopicStore) read(topicID string) (queuetopic.Topic, bool, error) {
	payload, err := s.db.Get(topicKey(topicID))
	if errors.Is(err, pebblerepo.ErrNotFound) {
		return queuetopic.Topic{}, false, nil
	}
	if err != nil {
		return queuetopic.Topic{}, false, fmt.Errorf("read queue topic %q: %w", topicID, err)
	}
	var topic queuetopic.Topic
	if err := json.Unmarshal(payload, &topic); err != nil {
		return queuetopic.Topic{}, false, fmt.Errorf("decode queue topic %q: %w", topicID, err)
	}
	if topic.TopicID != topicID || queuetopic.PhysicalID(topic.TenantID, topic.TopicName) != topicID {
		return queuetopic.Topic{}, false, fmt.Errorf("decode queue topic %q: stored identity does not match key", topicID)
	}
	return topic, true, nil
}

func topicKey(topicID string) []byte {
	return []byte(topicKeyPrefix + topicID)
}

func (s *TopicStore) checkWrite(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.RequireWriteLeader()
}

func validateDesired(topic queuetopic.Topic) error {
	if err := queuetopic.ValidateIdentity(topic.TenantID, topic.TopicName); err != nil {
		return err
	}
	if topic.TopicID != queuetopic.PhysicalID(topic.TenantID, topic.TopicName) {
		return fmt.Errorf("queue topic identity does not match physical id")
	}
	return nil
}
