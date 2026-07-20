package topics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
)

const testDeadLetterTopic = "events-dlq"

type fakeStore struct {
	topic   queuetopic.Topic
	err     error
	created bool
	changed bool
}

func (s *fakeStore) Upsert(_ context.Context, topic queuetopic.Topic) (queuetopic.Topic, bool, bool, error) {
	s.topic = topic
	return topic, s.created, s.changed, s.err
}

func (s *fakeStore) Get(_ context.Context, tenantID, topicName string) (queuetopic.Topic, error) {
	if s.err != nil {
		return queuetopic.Topic{}, s.err
	}
	return queuetopic.Topic{TenantID: tenantID, TopicName: topicName}, nil
}

func (s *fakeStore) Delete(_ context.Context, tenantID, topicName string) error {
	s.topic = queuetopic.Topic{TenantID: tenantID, TopicName: topicName}
	return s.err
}

func TestServiceUseCases(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{created: true}
	service := NewService(store, func() time.Time { return now })
	policy := queuetopic.Policy{PriorityTiers: []int{3, 1}, MaxAttempts: 5, DeadLetterTopicRef: testDeadLetterTopic}

	topic, created, changed, err := service.Upsert(context.Background(), "payments", "events", policy)
	if err != nil || !created || changed || topic.TopicID != "payments.events" {
		t.Fatalf("Upsert() = %#v, %t, %t, %v", topic, created, changed, err)
	}
	if _, _, _, err := service.Upsert(context.Background(), "bad tenant", "events", policy); err == nil {
		t.Fatal("Upsert() accepted invalid tenant")
	}

	got, err := service.Get(context.Background(), "payments", "events")
	if err != nil || got.TopicName != "events" {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	if _, err := service.Get(context.Background(), "payments", "bad.topic"); err == nil {
		t.Fatal("Get() accepted invalid topic")
	}
	if _, err := service.Get(context.Background(), "payments", "validation-dlq"); err != nil {
		t.Fatalf("Get(validation-dlq) error = %v", err)
	}

	if err := service.Delete(context.Background(), "payments", "events"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if store.topic.TopicName != "events" {
		t.Fatalf("Delete() topic = %#v", store.topic)
	}
	if err := service.Delete(context.Background(), "payments", "bad.topic"); err == nil {
		t.Fatal("Delete() accepted invalid topic")
	}
}

func TestServicePropagatesStoreErrors(t *testing.T) {
	want := errors.New("storage failed")
	store := &fakeStore{err: want}
	service := NewService(store, nil)
	policy := queuetopic.Policy{PriorityTiers: []int{0}, MaxAttempts: 1, DeadLetterTopicRef: testDeadLetterTopic}

	if _, _, _, err := service.Upsert(context.Background(), "payments", "events", policy); !errors.Is(err, want) {
		t.Fatalf("Upsert() error = %v", err)
	}
	if _, err := service.Get(context.Background(), "payments", "events"); !errors.Is(err, want) {
		t.Fatalf("Get() error = %v", err)
	}
	if err := service.Delete(context.Background(), "payments", "events"); !errors.Is(err, want) {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestUnavailableServiceFailsClosed(t *testing.T) {
	service := NewUnavailableService("raft replication unavailable")
	policy := queuetopic.Policy{PriorityTiers: []int{0}, MaxAttempts: 1, DeadLetterTopicRef: testDeadLetterTopic}

	_, _, _, upsertErr := service.Upsert(context.Background(), "payments", "events", policy)
	_, getErr := service.Get(context.Background(), "payments", "events")
	deleteErr := service.Delete(context.Background(), "payments", "events")
	for _, err := range []error{upsertErr, getErr, deleteErr} {
		var unavailable *queuetopic.UnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("error = %v, want UnavailableError", err)
		}
	}
}
