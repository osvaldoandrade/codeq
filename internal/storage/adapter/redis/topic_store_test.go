package redis

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	goredis "github.com/go-redis/redis/v8"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
)

func TestTopicStoreLifecycle(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewTopicStore(client)
	ctx := context.Background()
	createdAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	desired := validTopic(t, createdAt)

	created, wasCreated, changed, err := store.Upsert(ctx, desired)
	if err != nil || !wasCreated || changed || created.Version != 1 {
		t.Fatalf("create = %#v, %t, %t, %v", created, wasCreated, changed, err)
	}
	indexed, err := server.SIsMember(topicIndexKey, desired.TopicID)
	if err != nil || !indexed {
		t.Fatal("topic index does not contain created topic")
	}

	identical, wasCreated, changed, err := store.Upsert(ctx, desired)
	if err != nil || wasCreated || changed || identical.Version != 1 {
		t.Fatalf("idempotent upsert = %#v, %t, %t, %v", identical, wasCreated, changed, err)
	}

	desired.Policy.MaxConsumers = 40
	desired.UpdatedAt = createdAt.Add(time.Minute)
	updated, wasCreated, changed, err := store.Upsert(ctx, desired)
	if err != nil || wasCreated || !changed || updated.Version != 2 {
		t.Fatalf("update = %#v, %t, %t, %v", updated, wasCreated, changed, err)
	}
	if !updated.CreatedAt.Equal(createdAt) || !updated.UpdatedAt.Equal(createdAt.Add(time.Minute)) {
		t.Fatalf("updated timestamps = %s / %s", updated.CreatedAt, updated.UpdatedAt)
	}

	got, err := store.Get(ctx, "payments", "events")
	if err != nil || got.Version != 2 || got.Policy.MaxConsumers != 40 {
		t.Fatalf("Get() = %#v, %v", got, err)
	}

	if err := store.Delete(ctx, "payments", "events"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "payments", "events"); err != nil {
		t.Fatalf("idempotent Delete() error = %v", err)
	}
	_, err = store.Get(ctx, "payments", "events")
	var notFound *queuetopic.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Get() error = %v, want NotFoundError", err)
	}
}

func TestTopicStoreReportsBackendAndDataErrors(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	store := NewTopicStore(client)
	ctx := context.Background()

	if err := server.Set(topicKeyPrefix+"payments.events", "not-json"); err != nil {
		t.Fatalf("Set() malformed fixture error = %v", err)
	}
	_, err := store.Get(ctx, "payments", "events")
	if err == nil || !strings.Contains(err.Error(), "decode queue topic") {
		t.Fatalf("Get() malformed error = %v", err)
	}
	if err := server.Set(topicKeyPrefix+"payments.events", `{"topicId":"other.events","tenantId":"other","topicName":"events"}`); err != nil {
		t.Fatalf("Set() identity fixture error = %v", err)
	}
	_, err = store.Get(ctx, "payments", "events")
	if err == nil || !strings.Contains(err.Error(), "stored identity does not match key") {
		t.Fatalf("Get() identity error = %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := store.Get(ctx, "payments", "other"); err == nil {
		t.Fatal("Get() succeeded with closed client")
	}
	if err := store.Delete(ctx, "payments", "other"); err == nil {
		t.Fatal("Delete() succeeded with closed client")
	}
	if _, _, _, err := store.Upsert(ctx, validTopic(t, time.Now())); err == nil {
		t.Fatal("Upsert() succeeded with closed client")
	}
}

func validTopic(t *testing.T, now time.Time) queuetopic.Topic {
	t.Helper()
	topic, err := queuetopic.New("payments", "events", queuetopic.Policy{
		PriorityTiers:      []int{0, 3, 5},
		MaxAttempts:        5,
		DeadLetterTopicRef: "events-dlq",
		RetentionSeconds:   3600,
		MaxConsumers:       20,
	}, now)
	if err != nil {
		t.Fatalf("queuetopic.New() error = %v", err)
	}
	return topic
}
