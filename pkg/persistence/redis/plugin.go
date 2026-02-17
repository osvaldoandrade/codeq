package redis

import (
	"context"
	"encoding/json"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/persistence"

	"github.com/go-redis/redis/v8"
)

// Config holds Redis-specific configuration
type Config struct {
	Addr     string `json:"addr"`
	Password string `json:"password,omitempty"`
}

// Plugin implements PluginPersistence for Redis/KVRocks
type Plugin struct {
	client           *redis.Client
	taskRepo         repository.TaskRepository
	resultRepo       repository.ResultRepository
	subscriptionRepo repository.SubscriptionRepository
}

// NewPlugin creates a new Redis persistence plugin
func NewPlugin(config persistence.PluginConfig) (persistence.PluginPersistence, error) {
	var cfg Config
	if err := json.Unmarshal(config.Config, &cfg); err != nil {
		return nil, err
	}

	// Create Redis client
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
	})

	// Initialize repositories using existing implementations
	taskRepo := repository.NewTaskRepository(
		client,
		config.Timezone,
		config.BackoffPolicy,
		config.BackoffBaseSeconds,
		config.BackoffMaxSeconds,
	)
	
	resultRepo := repository.NewResultRepository(client, config.Timezone)
	subscriptionRepo := repository.NewSubscriptionRepository(client, config.Timezone)

	return &Plugin{
		client:           client,
		taskRepo:         taskRepo,
		resultRepo:       resultRepo,
		subscriptionRepo: subscriptionRepo,
	}, nil
}

// TaskStorage returns the task storage implementation
func (p *Plugin) TaskStorage() persistence.TaskStorage {
	return newTaskStorageAdapter(p.taskRepo)
}

// ResultStorage returns the result storage implementation
func (p *Plugin) ResultStorage() persistence.ResultStorage {
	return &resultStorageAdapter{repo: p.resultRepo}
}

// SubscriptionStorage returns the subscription storage implementation
func (p *Plugin) SubscriptionStorage() persistence.SubscriptionStorage {
	return newSubscriptionStorageAdapter(p.subscriptionRepo)
}

// Health checks if Redis is healthy
func (p *Plugin) Health(ctx context.Context) error {
	return p.client.Ping(ctx).Err()
}

// Close releases Redis connection
func (p *Plugin) Close() error {
	return p.client.Close()
}

func init() {
	persistence.RegisterProvider("redis", NewPlugin)
}
