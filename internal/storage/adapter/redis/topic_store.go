// Package redis implements storage adapters backed by Redis-compatible stores.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	goredis "github.com/go-redis/redis/v8"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
)

const (
	topicKeyPrefix = "codeq:admin:topic:"
	topicIndexKey  = "codeq:admin:topics"
	watchRetries   = 5
)

// TopicStore persists tenant-scoped topic policies using optimistic locking.
type TopicStore struct {
	client *goredis.Client
}

// NewTopicStore builds a Redis topic store over the application's shared client.
func NewTopicStore(client *goredis.Client) *TopicStore {
	return &TopicStore{client: client}
}

// Upsert atomically creates, updates, or confirms an identical topic policy.
func (s *TopicStore) Upsert(ctx context.Context, desired queuetopic.Topic) (queuetopic.Topic, bool, bool, error) {
	key := topicKeyPrefix + desired.TopicID
	for attempt := 0; attempt < watchRetries; attempt++ {
		stored, created, changed, err := s.upsertOnce(ctx, key, desired)
		if !errors.Is(err, goredis.TxFailedErr) {
			return stored, created, changed, err
		}
	}
	return queuetopic.Topic{}, false, false, &queuetopic.ConflictError{TopicID: desired.TopicID}
}

func (s *TopicStore) upsertOnce(ctx context.Context, key string, desired queuetopic.Topic) (stored queuetopic.Topic, created bool, changed bool, err error) {
	err = s.client.Watch(ctx, func(tx *goredis.Tx) error {
		current, exists, getErr := readTopic(ctx, tx, key, desired.TopicID)
		if getErr != nil {
			return getErr
		}
		if exists && queuetopic.SamePolicy(current.Policy, desired.Policy) {
			stored = current
			return nil
		}

		created = !exists
		changed = exists
		if exists {
			desired.CreatedAt = current.CreatedAt
			desired.Version = current.Version + 1
		} else {
			desired.Version = 1
		}
		payload, marshalErr := json.Marshal(desired)
		if marshalErr != nil {
			return fmt.Errorf("marshal queue topic: %w", marshalErr)
		}
		_, writeErr := tx.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
			pipe.Set(ctx, key, payload, 0)
			pipe.SAdd(ctx, topicIndexKey, desired.TopicID)
			return nil
		})
		if writeErr == nil {
			stored = desired
		}
		return writeErr
	}, key)
	return stored, created, changed, err
}

// Get returns one topic within the authenticated tenant.
func (s *TopicStore) Get(ctx context.Context, tenantID, topicName string) (queuetopic.Topic, error) {
	topicID := queuetopic.PhysicalID(tenantID, topicName)
	topic, exists, err := readTopic(ctx, s.client, topicKeyPrefix+topicID, topicID)
	if err != nil {
		return queuetopic.Topic{}, err
	}
	if !exists {
		return queuetopic.Topic{}, &queuetopic.NotFoundError{TopicID: topicID}
	}
	return topic, nil
}

// Delete removes a topic catalog entry idempotently.
func (s *TopicStore) Delete(ctx context.Context, tenantID, topicName string) error {
	topicID := queuetopic.PhysicalID(tenantID, topicName)
	_, err := s.client.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.Del(ctx, topicKeyPrefix+topicID)
		pipe.SRem(ctx, topicIndexKey, topicID)
		return nil
	})
	return err
}

type topicReader interface {
	Get(context.Context, string) *goredis.StringCmd
}

func readTopic(ctx context.Context, reader topicReader, key, topicID string) (queuetopic.Topic, bool, error) {
	payload, err := reader.Get(ctx, key).Bytes()
	if errors.Is(err, goredis.Nil) {
		return queuetopic.Topic{}, false, nil
	}
	if err != nil {
		return queuetopic.Topic{}, false, err
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
