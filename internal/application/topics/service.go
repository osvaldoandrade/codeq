// Package topics implements QueueTopic administration use cases.
package topics

import (
	"context"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
)

// Store is the consumer-owned persistence boundary for QueueTopic policies.
type Store interface {
	Upsert(context.Context, queuetopic.Topic) (topic queuetopic.Topic, created bool, changed bool, err error)
	Get(context.Context, string, string) (queuetopic.Topic, error)
	Delete(context.Context, string, string) error
}

// Service coordinates validation and persistence without transport concerns.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService builds a topic administration service.
func NewService(store Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}
}

// Upsert creates or reconciles a topic policy idempotently.
func (s *Service) Upsert(ctx context.Context, tenantID, topicName string, policy queuetopic.Policy) (queuetopic.Topic, bool, bool, error) {
	desired, err := queuetopic.New(tenantID, topicName, policy, s.now())
	if err != nil {
		return queuetopic.Topic{}, false, false, err
	}
	return s.store.Upsert(ctx, desired)
}

// Get returns a tenant-scoped topic.
func (s *Service) Get(ctx context.Context, tenantID, topicName string) (queuetopic.Topic, error) {
	if err := queuetopic.ValidateIdentity(tenantID, topicName); err != nil {
		return queuetopic.Topic{}, err
	}
	return s.store.Get(ctx, tenantID, topicName)
}

// Delete removes the provider catalog entry. The HTTP boundary requires an
// explicit deletion policy before this method is called.
func (s *Service) Delete(ctx context.Context, tenantID, topicName string) error {
	if err := queuetopic.ValidateIdentity(tenantID, topicName); err != nil {
		return err
	}
	return s.store.Delete(ctx, tenantID, topicName)
}
