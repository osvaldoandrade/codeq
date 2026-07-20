// Package pebble implements QueueTopic persistence on the embedded Pebble DB.
package pebble

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

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
	db database
	mu sync.Mutex
}

// NewTopicStore builds a tenant-scoped topic catalog over Pebble.
func NewTopicStore(db *pebblerepo.DB) *TopicStore {
	return &TopicStore{db: db}
}

// Upsert atomically creates, updates, or confirms an identical topic policy.
func (s *TopicStore) Upsert(ctx context.Context, desired queuetopic.Topic) (queuetopic.Topic, bool, bool, error) {
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
	if err := s.checkWrite(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWrite(ctx); err != nil {
		return err
	}
	topicID := queuetopic.PhysicalID(tenantID, topicName)
	if err := s.db.Delete(topicKey(topicID)); err != nil {
		return fmt.Errorf("delete queue topic %q: %w", topicID, err)
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
