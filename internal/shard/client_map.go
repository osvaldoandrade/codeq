package shard

import (
	"fmt"

	"github.com/go-redis/redis/v8"
)

// ClientMap holds a map of shard identifiers to Redis clients.
// It provides shard-aware client selection for multi-backend deployments.
type ClientMap struct {
	clients      map[string]*redis.Client
	defaultShard string
}

// NewClientMap creates a ClientMap from a map of shard identifiers to Redis clients.
// The defaultShard must exist in the clients map.
func NewClientMap(clients map[string]*redis.Client, defaultShard string) (*ClientMap, error) {
	if len(clients) == 0 {
		return nil, fmt.Errorf("at least one shard client is required")
	}
	if _, ok := clients[defaultShard]; !ok {
		return nil, fmt.Errorf("default shard %q not found in client map", defaultShard)
	}
	// Defensive copy
	m := make(map[string]*redis.Client, len(clients))
	for k, v := range clients {
		m[k] = v
	}
	return &ClientMap{clients: m, defaultShard: defaultShard}, nil
}

// NewSingleClientMap creates a ClientMap backed by a single Redis client.
// All shard IDs resolve to the same client (backward-compatible mode).
func NewSingleClientMap(client *redis.Client) *ClientMap {
	return &ClientMap{
		clients:      map[string]*redis.Client{DefaultShardID: client},
		defaultShard: DefaultShardID,
	}
}

// Client returns the Redis client for the given shard identifier.
// Returns the default shard client if the shard is not found.
func (m *ClientMap) Client(shardID string) *redis.Client {
	if c, ok := m.clients[shardID]; ok {
		return c
	}
	return m.clients[m.defaultShard]
}

// DefaultClient returns the Redis client for the default shard.
func (m *ClientMap) DefaultClient() *redis.Client {
	return m.clients[m.defaultShard]
}

// DefaultShard returns the default shard identifier.
func (m *ClientMap) DefaultShard() string {
	return m.defaultShard
}

// HasShard reports whether the given shard identifier exists in the map.
func (m *ClientMap) HasShard(shardID string) bool {
	_, ok := m.clients[shardID]
	return ok
}

// ShardIDs returns all shard identifiers in the map.
func (m *ClientMap) ShardIDs() []string {
	ids := make([]string, 0, len(m.clients))
	for k := range m.clients {
		ids = append(ids, k)
	}
	return ids
}

// Close closes all Redis clients in the map.
func (m *ClientMap) Close() error {
	var firstErr error
	for _, c := range m.clients {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
