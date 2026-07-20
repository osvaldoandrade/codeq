package pebble

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
)

var testNow = time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)

func TestTopicStoreLifecycleRestartAndTenantIsolation(t *testing.T) {
	dir := t.TempDir()
	db, err := pebblerepo.Open(pebblerepo.Options{Path: dir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store := NewTopicStore(db)
	desired := mustTopic(t, "acme", 20, testNow)

	created, wasCreated, changed, err := store.Upsert(context.Background(), desired)
	if err != nil || !wasCreated || changed || created.Version != 1 {
		t.Fatalf("create = %#v created=%v changed=%v err=%v", created, wasCreated, changed, err)
	}
	replayed, wasCreated, changed, err := store.Upsert(context.Background(), desired)
	if err != nil || wasCreated || changed || replayed.Version != 1 || !replayed.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("replay = %#v created=%v changed=%v err=%v", replayed, wasCreated, changed, err)
	}

	updatedDesired := mustTopic(t, "acme", 40, testNow.Add(time.Minute))
	updated, wasCreated, changed, err := store.Upsert(context.Background(), updatedDesired)
	if err != nil || wasCreated || !changed || updated.Version != 2 || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("update = %#v created=%v changed=%v err=%v", updated, wasCreated, changed, err)
	}

	other := mustTopic(t, "other", 7, testNow)
	if _, _, _, err := store.Upsert(context.Background(), other); err != nil {
		t.Fatalf("other tenant create: %v", err)
	}
	if got, err := store.Get(context.Background(), "other", "events"); err != nil || got.TopicID != "other.events" {
		t.Fatalf("other tenant get = %#v err=%v", got, err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	db, err = pebblerepo.Open(pebblerepo.Options{Path: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	store = NewTopicStore(db)
	restarted, err := store.Get(context.Background(), "acme", "events")
	if err != nil || restarted.Version != 2 || restarted.Policy.MaxConsumers != 40 {
		t.Fatalf("after restart = %#v err=%v", restarted, err)
	}
	if err := store.Delete(context.Background(), "acme", "events"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Delete(context.Background(), "acme", "events"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if _, err := store.Get(context.Background(), "acme", "events"); !isNotFound(err) {
		t.Fatalf("get deleted error = %v", err)
	}
	if got, err := store.Get(context.Background(), "other", "events"); err != nil || got.TopicID != "other.events" {
		t.Fatalf("tenant isolation after delete = %#v err=%v", got, err)
	}
}

func TestTopicStoreRejectsFollowerBeforeReplayOrDelete(t *testing.T) {
	hint := &testLeaderError{url: "http://leader:8080"}
	db := newMemoryDB()
	store := &TopicStore{db: db}
	desired := mustTopic(t, "acme", 20, testNow)
	if _, _, _, err := store.Upsert(context.Background(), desired); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.gateErr = hint

	if _, _, _, err := store.Upsert(context.Background(), desired); !errors.Is(err, hint) {
		t.Fatalf("follower replay error = %v", err)
	}
	if err := store.Delete(context.Background(), "acme", "events"); !errors.Is(err, hint) {
		t.Fatalf("follower delete error = %v", err)
	}
	if db.setCalls != 1 || db.deleteCalls != 0 {
		t.Fatalf("follower reached writes: set=%d delete=%d", db.setCalls, db.deleteCalls)
	}
}

func TestTopicStoreConcurrentUpdatesAreSerialized(t *testing.T) {
	db := newMemoryDB()
	store := &TopicStore{db: db}
	if _, _, _, err := store.Upsert(context.Background(), mustTopic(t, "acme", 1, testNow)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const updates = 32
	errCh := make(chan error, updates)
	var wg sync.WaitGroup
	for i := 0; i < updates; i++ {
		wg.Add(1)
		go func(maxConsumers int) {
			defer wg.Done()
			desired, err := queuetopic.New("acme", "events", testPolicy(maxConsumers), testNow.Add(time.Duration(maxConsumers)*time.Second))
			if err != nil {
				errCh <- err
				return
			}
			_, _, _, err = store.Upsert(context.Background(), desired)
			errCh <- err
		}(i + 2)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	got, err := store.Get(context.Background(), "acme", "events")
	if err != nil || got.Version != updates+1 {
		t.Fatalf("final topic = %#v err=%v", got, err)
	}
}

func TestTopicStoreContextCorruptionAndFaults(t *testing.T) {
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		store := &TopicStore{db: newMemoryDB()}
		if _, _, _, err := store.Upsert(ctx, mustTopic(t, "acme", 1, testNow)); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("stored identity mismatch", func(t *testing.T) {
		db := newMemoryDB()
		db.data[string(topicKey("acme.events"))] = []byte(`{"topicId":"other.events","tenantId":"other","topicName":"events"}`)
		store := &TopicStore{db: db}
		if _, err := store.Get(context.Background(), "acme", "events"); err == nil {
			t.Fatal("expected identity error")
		}
	})

	for _, tc := range []struct {
		name string
		set  func(*memoryDB)
		run  func(*TopicStore) error
	}{
		{name: "read", set: func(db *memoryDB) { db.getErr = errors.New("read fault") }, run: func(s *TopicStore) error {
			_, err := s.Get(context.Background(), "acme", "events")
			return err
		}},
		{name: "write", set: func(db *memoryDB) { db.setErr = errors.New("write fault") }, run: func(s *TopicStore) error {
			_, _, _, err := s.Upsert(context.Background(), mustTopic(t, "acme", 1, testNow))
			return err
		}},
		{name: "delete", set: func(db *memoryDB) { db.deleteErr = errors.New("delete fault") }, run: func(s *TopicStore) error {
			return s.Delete(context.Background(), "acme", "events")
		}},
	} {
		t.Run(tc.name+" fault", func(t *testing.T) {
			db := newMemoryDB()
			tc.set(db)
			if err := tc.run(&TopicStore{db: db}); err == nil {
				t.Fatal("expected backend error")
			}
		})
	}
}

func mustTopic(t *testing.T, tenantID string, maxConsumers int, now time.Time) queuetopic.Topic {
	t.Helper()
	topic, err := queuetopic.New(tenantID, "events", testPolicy(maxConsumers), now)
	if err != nil {
		t.Fatalf("new topic: %v", err)
	}
	return topic
}

func testPolicy(maxConsumers int) queuetopic.Policy {
	return queuetopic.Policy{PriorityTiers: []int{0, 3}, MaxAttempts: 5, DeadLetterTopicRef: "events-dlq", MaxConsumers: maxConsumers}
}

func isNotFound(err error) bool {
	var target *queuetopic.NotFoundError
	return errors.As(err, &target)
}

type testLeaderError struct{ url string }

func (e *testLeaderError) Error() string          { return "not leader" }
func (e *testLeaderError) LeaderHTTPAddr() string { return e.url }

type memoryDB struct {
	mu          sync.Mutex
	data        map[string][]byte
	gateErr     error
	getErr      error
	setErr      error
	deleteErr   error
	setCalls    int
	deleteCalls int
}

func newMemoryDB() *memoryDB { return &memoryDB{data: make(map[string][]byte)} }

func (db *memoryDB) RequireWriteLeader() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.gateErr
}

func (db *memoryDB) Get(key []byte) ([]byte, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.getErr != nil {
		return nil, db.getErr
	}
	value, ok := db.data[string(key)]
	if !ok {
		return nil, pebblerepo.ErrNotFound
	}
	return append([]byte(nil), value...), nil
}

func (db *memoryDB) Set(key, value []byte) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.setCalls++
	if db.setErr != nil {
		return db.setErr
	}
	db.data[string(key)] = append([]byte(nil), value...)
	return nil
}

func (db *memoryDB) Delete(key []byte) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.deleteCalls++
	if db.deleteErr != nil {
		return db.deleteErr
	}
	delete(db.data, string(key))
	return nil
}

var (
	_ database = (*memoryDB)(nil)
	_ error    = (*testLeaderError)(nil)
)
