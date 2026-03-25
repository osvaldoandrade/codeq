package shard

import (
	"context"
	"testing"
)

func TestNewDefaultShardSupplier(t *testing.T) {
	s := NewDefaultShardSupplier()
	ctx := context.Background()

	shard, err := s.CurrentShard(ctx, "GENERATE_MASTER", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shard != DefaultShardID {
		t.Errorf("expected %q, got %q", DefaultShardID, shard)
	}

	shards, err := s.QueueShards(ctx, "GENERATE_MASTER", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(shards) != 1 || shards[0] != DefaultShardID {
		t.Errorf("expected [%q], got %v", DefaultShardID, shards)
	}
}

func TestStaticShardSupplier_DefaultShardFallback(t *testing.T) {
	s, err := NewStaticShardSupplier(StaticConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()

	shard, err := s.CurrentShard(ctx, "UNKNOWN_CMD", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shard != DefaultShardID {
		t.Errorf("expected %q, got %q", DefaultShardID, shard)
	}
}

func TestStaticShardSupplier_CommandMapping(t *testing.T) {
	s, err := NewStaticShardSupplier(StaticConfig{
		DefaultShard: "primary",
		CommandMappings: map[string]string{
			"GENERATE_MASTER":   "compute-heavy",
			"GENERATE_CREATIVE": "compute-heavy",
			"SEND_EMAIL":        "notification",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()

	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"mapped command", "GENERATE_MASTER", "compute-heavy"},
		{"mapped command 2", "SEND_EMAIL", "notification"},
		{"unmapped command falls back to default", "UNKNOWN", "primary"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shard, err := s.CurrentShard(ctx, tt.command, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if shard != tt.want {
				t.Errorf("CurrentShard(%q) = %q, want %q", tt.command, shard, tt.want)
			}
		})
	}
}

func TestStaticShardSupplier_CaseInsensitiveCommand(t *testing.T) {
	s, err := NewStaticShardSupplier(StaticConfig{
		DefaultShard: "primary",
		CommandMappings: map[string]string{
			"GENERATE_MASTER": "compute",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()

	shard, err := s.CurrentShard(ctx, "generate_master", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shard != "compute" {
		t.Errorf("expected %q, got %q", "compute", shard)
	}
}

func TestStaticShardSupplier_TenantOverride(t *testing.T) {
	s, err := NewStaticShardSupplier(StaticConfig{
		DefaultShard: "primary",
		CommandMappings: map[string]string{
			"GENERATE_MASTER": "compute",
		},
		TenantOverrides: map[string]string{
			"tenant-premium": "premium-shard",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()

	// Tenant override should take priority over command mapping
	shard, err := s.CurrentShard(ctx, "GENERATE_MASTER", "tenant-premium")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shard != "premium-shard" {
		t.Errorf("expected %q, got %q", "premium-shard", shard)
	}

	// Non-premium tenant should use command mapping
	shard, err = s.CurrentShard(ctx, "GENERATE_MASTER", "tenant-standard")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shard != "compute" {
		t.Errorf("expected %q, got %q", "compute", shard)
	}
}

func TestStaticShardSupplier_QueueShards_MultiShard(t *testing.T) {
	s, err := NewStaticShardSupplier(StaticConfig{
		DefaultShard: "primary",
		CommandMappings: map[string]string{
			"GENERATE_MASTER": "compute",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()

	// Command mapped to non-default shard should return both
	shards, err := s.QueueShards(ctx, "GENERATE_MASTER", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("expected 2 shards, got %d: %v", len(shards), shards)
	}
	if shards[0] != "compute" {
		t.Errorf("expected first shard %q, got %q", "compute", shards[0])
	}
	if shards[1] != "primary" {
		t.Errorf("expected second shard %q, got %q", "primary", shards[1])
	}
}

func TestStaticShardSupplier_QueueShards_SingleShard(t *testing.T) {
	s, err := NewStaticShardSupplier(StaticConfig{
		DefaultShard: "primary",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()

	// Unmapped command should return only the default shard
	shards, err := s.QueueShards(ctx, "UNKNOWN", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(shards) != 1 || shards[0] != "primary" {
		t.Errorf("expected [primary], got %v", shards)
	}
}

func TestStaticShardSupplier_Validate(t *testing.T) {
	s, err := NewStaticShardSupplier(StaticConfig{
		DefaultShard: "primary",
		CommandMappings: map[string]string{
			"GENERATE_MASTER": "compute",
		},
		TenantOverrides: map[string]string{
			"tenant-premium": "premium",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Valid: all shards exist
	known := map[string]struct{}{
		"primary": {},
		"compute": {},
		"premium": {},
	}
	if err := s.Validate(known); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	// Missing default shard
	missing := map[string]struct{}{
		"compute": {},
		"premium": {},
	}
	if err := s.Validate(missing); err == nil {
		t.Error("expected error for missing default shard")
	}

	// Missing command mapping shard
	missingCmd := map[string]struct{}{
		"primary": {},
		"premium": {},
	}
	if err := s.Validate(missingCmd); err == nil {
		t.Error("expected error for missing command shard")
	}

	// Missing tenant override shard
	missingTenant := map[string]struct{}{
		"primary": {},
		"compute": {},
	}
	if err := s.Validate(missingTenant); err == nil {
		t.Error("expected error for missing tenant shard")
	}
}

func TestStaticShardSupplier_TwoOrMoreShards(t *testing.T) {
	// Validates the acceptance criterion: Support for 2+ shards validated
	s, err := NewStaticShardSupplier(StaticConfig{
		DefaultShard: "shard-a",
		CommandMappings: map[string]string{
			"CMD_1": "shard-b",
			"CMD_2": "shard-c",
		},
		TenantOverrides: map[string]string{
			"tenant-x": "shard-d",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()

	tests := []struct {
		name     string
		command  string
		tenantID string
		want     string
	}{
		{"default shard", "UNKNOWN", "", "shard-a"},
		{"command to shard-b", "CMD_1", "", "shard-b"},
		{"command to shard-c", "CMD_2", "", "shard-c"},
		{"tenant override to shard-d", "CMD_1", "tenant-x", "shard-d"},
		{"no tenant override, command mapping applies", "CMD_1", "tenant-y", "shard-b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.CurrentShard(ctx, tt.command, tt.tenantID)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("CurrentShard(%q, %q) = %q, want %q", tt.command, tt.tenantID, got, tt.want)
			}
		})
	}
}
