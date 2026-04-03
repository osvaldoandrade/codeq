package shard

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestNewClientMap_Valid(t *testing.T) {
	mr1, _ := miniredis.Run()
	mr2, _ := miniredis.Run()
	t.Cleanup(mr1.Close)
	t.Cleanup(mr2.Close)

	c1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	c2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	t.Cleanup(func() { _ = c2.Close() })

	cm, err := NewClientMap(map[string]*redis.Client{
		"primary": c1,
		"compute": c2,
	}, "primary")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cm.Client("primary") != c1 {
		t.Error("expected Client(primary) to return c1")
	}
	if cm.Client("compute") != c2 {
		t.Error("expected Client(compute) to return c2")
	}
	if cm.DefaultClient() != c1 {
		t.Error("expected DefaultClient to return c1")
	}
}

func TestNewClientMap_UnknownShardFallsBackToDefault(t *testing.T) {
	mr, _ := miniredis.Run()
	t.Cleanup(mr.Close)

	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	cm, err := NewClientMap(map[string]*redis.Client{
		"primary": c,
	}, "primary")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cm.Client("unknown-shard") != c {
		t.Error("expected unknown shard to fall back to default client")
	}
}

func TestNewClientMap_EmptyClientsError(t *testing.T) {
	_, err := NewClientMap(map[string]*redis.Client{}, "primary")
	if err == nil {
		t.Error("expected error for empty clients map")
	}
}

func TestNewClientMap_MissingDefaultError(t *testing.T) {
	mr, _ := miniredis.Run()
	t.Cleanup(mr.Close)

	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	_, err := NewClientMap(map[string]*redis.Client{"other": c}, "primary")
	if err == nil {
		t.Error("expected error when default shard not in map")
	}
}

func TestNewSingleClientMap(t *testing.T) {
	mr, _ := miniredis.Run()
	t.Cleanup(mr.Close)

	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })

	cm := NewSingleClientMap(c)

	if cm.DefaultClient() != c {
		t.Error("expected DefaultClient to return the single client")
	}
	// Any shard ID should resolve to the same client
	if cm.Client("any-shard") != c {
		t.Error("expected any shard to resolve to the single client")
	}
	if cm.Client(DefaultShardID) != c {
		t.Error("expected default shard to resolve to the single client")
	}
}

func TestClientMap_ShardIDs(t *testing.T) {
	mr1, _ := miniredis.Run()
	mr2, _ := miniredis.Run()
	t.Cleanup(mr1.Close)
	t.Cleanup(mr2.Close)

	c1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	c2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	t.Cleanup(func() { _ = c1.Close() })
	t.Cleanup(func() { _ = c2.Close() })

	cm, _ := NewClientMap(map[string]*redis.Client{
		"primary": c1,
		"compute": c2,
	}, "primary")

	ids := cm.ShardIDs()
	if len(ids) != 2 {
		t.Fatalf("expected 2 shard IDs, got %d", len(ids))
	}

	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen["primary"] || !seen["compute"] {
		t.Errorf("expected primary and compute in shard IDs, got %v", ids)
	}
}

func TestClientMap_Close(t *testing.T) {
	mr, _ := miniredis.Run()
	t.Cleanup(mr.Close)

	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cm := NewSingleClientMap(c)

	if err := cm.Close(); err != nil {
		t.Errorf("unexpected error on Close: %v", err)
	}
}
