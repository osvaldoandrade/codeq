package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/providers"
	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/internal/shard"
	"github.com/osvaldoandrade/codeq/internal/tracing"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
)

// RaftGroupStatus is the small slice of raft state the status endpoint
// + observability surfaces need. *internal/raft.DB satisfies it.
// Defined here (not in the raft package) so the public Application
// type avoids importing internal/raft transitively.
type RaftGroupStatus interface {
	IsLeader() bool
	SelfID() string
	BindAddr() string
	LeaderInfo() (id, addr string)
	LeaderHTTPAddr() string
}

type Application struct {
	Config            *config.Config
	Engine            *gin.Engine
	Scheduler         services.SchedulerService
	Results           services.ResultsService
	Subs              services.SubscriptionService
	Logger            *slog.Logger
	TZ                *time.Location
	ProducerValidator auth.Validator
	WorkerValidator   auth.Validator
	RateLimiter       ratelimit.Limiter
	// RaftGroups, when non-nil, is the per-shard raft state in raft
	// mode. Index = shardIdx. Empty when raft is disabled.
	RaftGroups      []RaftGroupStatus
	TracingShutdown func(context.Context) error
}

// ApplicationOption configures the Application
type ApplicationOption func(*Application) error

// WithProducerValidator sets a custom producer validator
func WithProducerValidator(validator auth.Validator) ApplicationOption {
	return func(app *Application) error {
		app.ProducerValidator = validator
		return nil
	}
}

// WithWorkerValidator sets a custom worker validator
func WithWorkerValidator(validator auth.Validator) ApplicationOption {
	return func(app *Application) error {
		app.WorkerValidator = validator
		return nil
	}
}

// newShardClient builds a per-shard go-redis client with the same connection
// hygiene as the primary provider (timeouts disabled to skip per-op time.Now()).
func newShardClient(addr, password string, db, poolSize int) (*redis.Client, error) {
	return redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     poolSize,
		ReadTimeout:  -1,
		WriteTimeout: -1,
		IdleTimeout:  0,
	}), nil
}

func NewApplication(cfg *config.Config, opts ...ApplicationOption) (*Application, error) {
	// Persistence dispatch happens *before* any Redis-specific setup so a
	// Pebble deployment doesn't need a reachable Redis at all. The Pebble
	// path owns its own ratelimiter (no-op) since there's no shared bucket
	// to enforce across processes.
	if cfg.PersistenceProvider == "pebble" {
		return newPebbleApplication(cfg, nil, noopLimiter{}, nil, nil, nil, nil, nil, opts...)
	}

	redisClient := providers.NewRedisProvider(cfg.RedisAddr, cfg.RedisPassword)
	if err := repository.PreloadScripts(context.Background(), redisClient); err != nil {
		return nil, fmt.Errorf("preload repository scripts: %w", err)
	}
	if err := ratelimit.PreloadScripts(context.Background(), redisClient); err != nil {
		return nil, fmt.Errorf("preload ratelimit scripts: %w", err)
	}
	limiter := ratelimit.NewTokenBucketLimiter(redisClient)

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.FixedZone("UTC", 0)
	}

	level := new(slog.LevelVar)
	switch cfg.LogLevel {
	case "debug":
		level.Set(slog.LevelDebug)
	case "warn":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		level.Set(slog.LevelInfo)
	}
	var handler slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	logger := slog.New(handler).With("service", "codeq", "env", cfg.Env)
	slog.SetDefault(logger)

	tracingShutdown, _ := tracing.Setup(context.Background(), tracing.Config{
		Enabled:      cfg.TracingEnabled,
		ServiceName:  cfg.TracingServiceName,
		OTLPEndpoint: cfg.TracingOtlpEndpoint,
		OTLPInsecure: cfg.TracingOtlpInsecure,
		SampleRatio:  cfg.TracingSampleRatio,
	}, logger)

	metrics.RegisterRedisCollector(redisClient, logger)

	webhookClient := &http.Client{
		Transport: http.DefaultTransport,
		Timeout:   15 * time.Second,
	}

	// Create ShardSupplier from sharding configuration
	var shardSupplier domain.ShardSupplier
	if cfg.Sharding.Enabled {
		supplier, err := shard.NewStaticShardSupplier(shard.StaticConfig{
			DefaultShard:    cfg.Sharding.DefaultShard,
			CommandMappings: cfg.Sharding.CommandMappings,
			TenantOverrides: cfg.Sharding.TenantOverrides,
		})
		if err != nil {
			return nil, fmt.Errorf("create shard supplier: %w", err)
		}
		shardSupplier = supplier
	} else {
		shardSupplier = shard.NewDefaultShardSupplier()
	}

	// Create task repository: use sharded repository when multiple backends are configured
	var repo repository.TaskRepository
	if cfg.Sharding.Enabled && len(cfg.Sharding.Backends) > 0 {
		clients := make(map[string]*redis.Client, len(cfg.Sharding.Backends))
		for name, backend := range cfg.Sharding.Backends {
			poolSize := backend.PoolSize
			if poolSize <= 0 {
				poolSize = 10
			}
			client, err := newShardClient(backend.Address, backend.Password, backend.DB, poolSize)
			if err != nil {
				return nil, fmt.Errorf("create shard client %s: %w", name, err)
			}
			if err := repository.PreloadScripts(context.Background(), client); err != nil {
				return nil, fmt.Errorf("preload repository scripts on shard %s: %w", name, err)
			}
			clients[name] = client
		}
		defaultShard := cfg.Sharding.DefaultShard
		if defaultShard == "" {
			defaultShard = shard.DefaultShardID
		}
		clientMap, err := shard.NewClientMap(clients, defaultShard)
		if err != nil {
			return nil, fmt.Errorf("create shard client map: %w", err)
		}
		repo = repository.NewShardedTaskRepository(clientMap, loc, cfg.BackoffPolicy, cfg.BackoffBaseSeconds, cfg.BackoffMaxSeconds, shardSupplier)
	} else {
		repo = repository.NewTaskRepository(redisClient, loc, cfg.BackoffPolicy, cfg.BackoffBaseSeconds, cfg.BackoffMaxSeconds, shardSupplier)
	}
	subRepo := repository.NewSubscriptionRepository(redisClient, loc)
	subs := services.NewSubscriptionService(subRepo)
	notifier := services.NewNotifierService(subRepo, logger, cfg.WebhookHmacSecret, cfg.SubscriptionMinIntervalSeconds, limiter, ratelimit.Bucket(cfg.RateLimit.Webhook), webhookClient)
	cleanup := services.NewSubscriptionCleanupService(subRepo, logger, cfg.SubscriptionCleanupIntervalSeconds)
	resultCallback := services.NewResultCallbackService(
		logger,
		cfg.WebhookHmacSecret,
		cfg.ResultWebhookMaxAttempts,
		cfg.ResultWebhookBaseBackoffSeconds,
		cfg.ResultWebhookMaxBackoffSeconds,
		limiter,
		ratelimit.Bucket(cfg.RateLimit.Webhook),
		webhookClient,
	)
	scheduler := services.NewSchedulerService(
		repo,
		notifier,
		resultCallback,
		loc,
		time.Now,
		cfg.DefaultLeaseSeconds,
		cfg.RequeueInspectLimit,
		cfg.MaxAttemptsDefault,
		cfg.BackoffPolicy,
		cfg.BackoffBaseSeconds,
		cfg.BackoffMaxSeconds,
	)
	var resultRepo repository.ResultRepository
	if cfg.Sharding.Enabled && len(cfg.Sharding.Backends) > 0 {
		resultClients := make(map[string]*redis.Client, len(cfg.Sharding.Backends))
		for name, backend := range cfg.Sharding.Backends {
			poolSize := backend.PoolSize
			if poolSize <= 0 {
				poolSize = 10
			}
			c, err := newShardClient(backend.Address, backend.Password, backend.DB, poolSize)
			if err != nil {
				return nil, fmt.Errorf("create result shard client %s: %w", name, err)
			}
			resultClients[name] = c
		}
		defaultShard := cfg.Sharding.DefaultShard
		if defaultShard == "" {
			defaultShard = shard.DefaultShardID
		}
		resultClientMap, err := shard.NewClientMap(resultClients, defaultShard)
		if err != nil {
			return nil, fmt.Errorf("create result shard client map: %w", err)
		}
		resultRepo = repository.NewShardedResultRepository(resultClientMap, loc, shardSupplier)
	} else {
		resultRepo = repository.NewResultRepository(redisClient, loc, shardSupplier)
	}
	uploader := providers.NewLocalUploader(cfg.LocalArtifactsDir)
	results := services.NewResultsService(resultRepo, uploader, resultCallback, logger, time.Now, loc)

	engine := gin.New()
	engine.Use(gin.Recovery(), middleware.RequestIDMiddleware())
	if cfg.TracingEnabled {
		engine.Use(middleware.TracingMiddleware(cfg.TracingServiceName))
	}
	engine.Use(middleware.LoggerMiddleware(logger))

	go cleanup.Start(context.Background())

	app := &Application{
		Config:      cfg,
		Engine:      engine,
		Scheduler:   scheduler,
		Results:     results,
		Subs:        subs,
		Logger:      logger,
		TZ:          loc,
		RateLimiter: limiter,
		TracingShutdown: func(ctx context.Context) error {
			if tracingShutdown == nil {
				return nil
			}
			return tracingShutdown(ctx)
		},
	}

	// Apply options
	for _, opt := range opts {
		if err := opt(app); err != nil {
			return nil, err
		}
	}

	// Create default validators from config if not provided
	if app.ProducerValidator == nil && cfg.ProducerAuthProvider != "" {
		validator, err := auth.NewValidator(auth.ProviderConfig{
			Type:   cfg.ProducerAuthProvider,
			Config: cfg.ProducerAuthConfig,
		})
		if err != nil {
			return nil, err
		}
		app.ProducerValidator = validator
	}

	if app.WorkerValidator == nil && cfg.WorkerAuthProvider != "" {
		validator, err := auth.NewValidator(auth.ProviderConfig{
			Type:   cfg.WorkerAuthProvider,
			Config: cfg.WorkerAuthConfig,
		})
		if err != nil {
			return nil, err
		}
		app.WorkerValidator = validator
	}

	workerStream, err := startWorkerStreamServer(
		cfg,
		scheduler,
		results,
		app.WorkerValidator,
		app.ProducerValidator,
		logger,
	)
	if err != nil {
		return nil, err
	}
	producerStream, err := startProducerStreamServer(
		cfg,
		scheduler,
		app.ProducerValidator,
		logger,
	)
	if err != nil {
		stopGRPCServer(context.Background(), workerStream)
		return nil, err
	}
	if workerStream != nil || producerStream != nil {
		tracingOnlyShutdown := app.TracingShutdown
		app.TracingShutdown = func(ctx context.Context) error {
			stopGRPCServer(ctx, workerStream)
			stopGRPCServer(ctx, producerStream)
			if tracingOnlyShutdown == nil {
				return nil
			}
			return tracingOnlyShutdown(ctx)
		}
	}

	return app, nil
}
