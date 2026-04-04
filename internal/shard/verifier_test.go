package shard

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestVerify_AfterMigration(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")

	// Seed source and migrate
	seedPendingTasks(ctx, t, src, "GENERATE_MASTER", "", "default", 0, 3)

	_, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "GENERATE_MASTER",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	vr, err := Verify(ctx, cm, "GENERATE_MASTER", "", "default", "compute-shard")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vr.OK {
		t.Errorf("expected OK=true, got false. source=%v dest=%v healthy=%v", vr.SourceCounts, vr.DestCounts, vr.Healthy)
	}
	if vr.SourceCounts["pending"] != 0 {
		t.Errorf("expected 0 pending on source, got %d", vr.SourceCounts["pending"])
	}
	if vr.DestCounts["pending"] != 3 {
		t.Errorf("expected 3 pending on dest, got %d", vr.DestCounts["pending"])
	}
}

func TestVerify_SourceNotEmpty(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")

	// Seed source but don't migrate
	seedPendingTasks(ctx, t, src, "GENERATE_MASTER", "", "default", 0, 5)

	vr, err := Verify(ctx, cm, "GENERATE_MASTER", "", "default", "compute-shard")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if vr.OK {
		t.Error("expected OK=false when source still has tasks")
	}
	if vr.SourceCounts["pending"] != 5 {
		t.Errorf("expected 5 pending on source, got %d", vr.SourceCounts["pending"])
	}
}

func TestHealthCheck_AllHealthy(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)

	health := HealthCheck(ctx, cm)
	for sid, ok := range health {
		if !ok {
			t.Errorf("shard %q expected healthy", sid)
		}
	}
}

func TestHealthCheck_UnhealthyShard(t *testing.T) {
	mr1, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr1.Close)

	c1 := redis.NewClient(&redis.Options{Addr: mr1.Addr()})
	// Create a client pointing to a closed server
	mr2, _ := miniredis.Run()
	c2 := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	mr2.Close() // Close immediately to make it unhealthy
	t.Cleanup(func() { _ = c1.Close() })
	t.Cleanup(func() { _ = c2.Close() })

	cm, _ := NewClientMap(map[string]*redis.Client{
		"default": c1,
		"broken":  c2,
	}, "default")

	ctx := context.Background()
	health := HealthCheck(ctx, cm)

	if !health["default"] {
		t.Error("expected default to be healthy")
	}
	if health["broken"] {
		t.Error("expected broken to be unhealthy")
	}
}

func TestVerify_AllQueueTypes(t *testing.T) {
	ctx, _, _, cm := setupMigrationClients(t)
	src := cm.Client("default")

	// Seed all queue types
	seedPendingTasks(ctx, t, src, "CMD", "", "default", 0, 2)
	seedDelayedTasks(ctx, t, src, "CMD", "", "default", 3)
	inprogKey := QueueKeyInProgress("CMD", "", "default")
	seedSetTasks(ctx, t, src, inprogKey, 1, "ip")
	dlqKey := QueueKeyDLQ("CMD", "", "default")
	seedSetTasks(ctx, t, src, dlqKey, 4, "dq")

	// Migrate everything
	_, err := Migrate(ctx, cm, MigrateOptions{
		Command:   "CMD",
		FromShard: "default",
		ToShard:   "compute-shard",
		BatchSize: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	vr, err := Verify(ctx, cm, "CMD", "", "default", "compute-shard")
	if err != nil {
		t.Fatal(err)
	}

	if !vr.OK {
		t.Errorf("expected OK=true after full migration. source=%v dest=%v", vr.SourceCounts, vr.DestCounts)
	}
	if vr.DestCounts["pending"] != 2 {
		t.Errorf("dest pending: got %d, want 2", vr.DestCounts["pending"])
	}
	if vr.DestCounts["delayed"] != 3 {
		t.Errorf("dest delayed: got %d, want 3", vr.DestCounts["delayed"])
	}
	if vr.DestCounts["inprog"] != 1 {
		t.Errorf("dest inprog: got %d, want 1", vr.DestCounts["inprog"])
	}
	if vr.DestCounts["dlq"] != 4 {
		t.Errorf("dest dlq: got %d, want 4", vr.DestCounts["dlq"])
	}
}
