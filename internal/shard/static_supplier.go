package shard

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// DefaultShardID is the shard identifier used for single-shard deployments.
const DefaultShardID = "default"

// StaticShardSupplier maps commands and tenants to shards using static configuration.
// Routing precedence: tenant overrides → command mappings → default shard.
// Thread-safe: configuration is immutable after construction.
type StaticShardSupplier struct {
	mu              sync.RWMutex
	defaultShard    string
	commandMappings map[string]string
	tenantOverrides map[string]string
}

// StaticConfig holds the static shard routing configuration.
type StaticConfig struct {
	DefaultShard    string
	CommandMappings map[string]string
	TenantOverrides map[string]string
}

// NewStaticShardSupplier creates a StaticShardSupplier from the given configuration.
// It returns an error if the default shard is empty.
func NewStaticShardSupplier(cfg StaticConfig) (*StaticShardSupplier, error) {
	defaultShard := cfg.DefaultShard
	if defaultShard == "" {
		defaultShard = DefaultShardID
	}

	cmdMap := make(map[string]string, len(cfg.CommandMappings))
	for k, v := range cfg.CommandMappings {
		cmdMap[strings.ToUpper(k)] = v
	}

	tenantMap := make(map[string]string, len(cfg.TenantOverrides))
	for k, v := range cfg.TenantOverrides {
		tenantMap[k] = v
	}

	return &StaticShardSupplier{
		defaultShard:    defaultShard,
		commandMappings: cmdMap,
		tenantOverrides: tenantMap,
	}, nil
}

// NewDefaultShardSupplier returns a single-shard supplier that routes everything to "default".
// This preserves backward compatibility for deployments without sharding configuration.
func NewDefaultShardSupplier() domain.ShardSupplier {
	return &StaticShardSupplier{
		defaultShard:    DefaultShardID,
		commandMappings: map[string]string{},
		tenantOverrides: map[string]string{},
	}
}

// CurrentShard returns the shard identifier for enqueue and claim operations.
// Routing precedence: tenant override → command mapping → default shard.
func (s *StaticShardSupplier) CurrentShard(_ context.Context, command string, tenantID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Tenant overrides take highest priority
	if tenantID != "" {
		if shardID, ok := s.tenantOverrides[tenantID]; ok {
			return shardID, nil
		}
	}

	// Command mappings
	upperCmd := strings.ToUpper(command)
	if shardID, ok := s.commandMappings[upperCmd]; ok {
		return shardID, nil
	}

	return s.defaultShard, nil
}

// QueueShards returns all shard identifiers where queues for this command-tenant may exist.
// During migrations this may return both old and new shards.
func (s *StaticShardSupplier) QueueShards(ctx context.Context, command string, tenantID string) ([]string, error) {
	current, err := s.CurrentShard(ctx, command, tenantID)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Collect unique shards: always include default and current
	seen := map[string]struct{}{current: {}}
	shards := []string{current}

	if s.defaultShard != current {
		seen[s.defaultShard] = struct{}{}
		shards = append(shards, s.defaultShard)
	}

	return shards, nil
}

// Validate checks that all shard references in the configuration refer to known shards.
// knownShards is the set of shard identifiers that have backend connections configured.
func (s *StaticShardSupplier) Validate(knownShards map[string]struct{}) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := knownShards[s.defaultShard]; !ok {
		return fmt.Errorf("default shard %q not found in configured backends", s.defaultShard)
	}

	for cmd, shardID := range s.commandMappings {
		if _, ok := knownShards[shardID]; !ok {
			return fmt.Errorf("command %s maps to undefined shard %q", cmd, shardID)
		}
	}

	for tenant, shardID := range s.tenantOverrides {
		if _, ok := knownShards[shardID]; !ok {
			return fmt.Errorf("tenant %s override maps to undefined shard %q", tenant, shardID)
		}
	}

	return nil
}
