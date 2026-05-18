package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"

	"net"

	"google.golang.org/grpc"

	"github.com/osvaldoandrade/codeq/internal/cluster"
	"github.com/osvaldoandrade/codeq/internal/cluster/clusterpb"
	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/providers"
	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/internal/repository"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// validateClusterConfig checks the static cluster config is well-formed
// before any side-effect (listen, dial) so misconfigurations fail fast.
func validateClusterConfig(c config.ClusterConfig) error {
	if c.SelfID == "" {
		return fmt.Errorf("cluster.selfId is required when cluster.enabled=true")
	}
	if c.GRPCAddr == "" {
		return fmt.Errorf("cluster.grpcAddr is required when cluster.enabled=true")
	}
	if len(c.Nodes) == 0 {
		return fmt.Errorf("cluster.nodes is empty")
	}
	seen := make(map[string]bool, len(c.Nodes))
	selfFound := false
	for _, n := range c.Nodes {
		if n.ID == "" || n.GRPCAddr == "" {
			return fmt.Errorf("cluster node missing id/grpcAddr: %+v", n)
		}
		if seen[n.ID] {
			return fmt.Errorf("cluster node id %q listed twice", n.ID)
		}
		seen[n.ID] = true
		if n.ID == c.SelfID {
			selfFound = true
		}
	}
	if !selfFound {
		return fmt.Errorf("cluster.selfId %q not present in cluster.nodes", c.SelfID)
	}
	return nil
}

// noopLimiter is the rate-limiter the Pebble path uses: there is no
// shared bucket across processes (Pebble is single-instance), and the
// in-flight benchmark doesn't exercise webhook rate limiting. A real
// deployment can replace this with an in-process token bucket without
// touching the redis path.
type noopLimiter struct{}

func (noopLimiter) Allow(_ context.Context, _ string, _ string, _ ratelimit.Bucket) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true}, nil
}

// pebbleConfig is the shape we expect when PersistenceProvider="pebble".
// "path" is the only required field; "fsyncOnCommit" can be flipped on for
// the durability-first tier (defaults to no-sync for max throughput).
type pebbleConfig struct {
	Path          string `json:"path"`
	FsyncOnCommit bool   `json:"fsyncOnCommit"`
	// NumShards enables Phase 8 single-node sharding. 0/1 keeps the
	// historical single-DB behaviour. N>1 opens N independent Pebble
	// instances under Path/shard<i>/ and routes every task's keys by
	// hash(task_id) % N. Compaction, commit pipeline, and write
	// concurrency all parallelise across shards.
	NumShards int `json:"numShards"`
}

// newPebbleApplication constructs the full Application stack against an
// embedded Pebble DB. It mirrors the redis NewApplication shape so the
// caller (NewApplication itself) can swap on cfg.PersistenceProvider with
// no other downstream changes.
//
// redisClient is still required for ratelimit + (currently) the
// notifier/subscription helpers that haven't been ported. They'll either
// be migrated in a follow-up or wired against an in-process token-bucket;
// for now we accept the dual dependency so Pebble can be enabled without
// rewriting half the system at once.
func newPebbleApplication(
	cfg *config.Config,
	redisClient *redis.Client,
	limiter ratelimit.Limiter,
	loc *time.Location,
	logger *slog.Logger,
	webhookClient *http.Client,
	tracingShutdown func(context.Context) error,
	shardSupplier domain.ShardSupplier,
	opts ...ApplicationOption,
) (*Application, error) {
	// Initialize anything the dispatch site couldn't pre-build for us; the
	// Pebble path is reachable both from NewApplication (where everything
	// would already be set) and directly in tests (everything nil).
	if loc == nil {
		l, err := time.LoadLocation(cfg.Timezone)
		if err != nil || l == nil {
			l = time.UTC
		}
		loc = l
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if webhookClient == nil {
		webhookClient = &http.Client{Transport: http.DefaultTransport, Timeout: 15 * time.Second}
	}

	var pc pebbleConfig
	if len(cfg.PersistenceConfig) > 0 {
		if err := json.Unmarshal(cfg.PersistenceConfig, &pc); err != nil {
			return nil, fmt.Errorf("parse pebble PersistenceConfig: %w", err)
		}
	}
	if pc.Path == "" {
		pc.Path = "./codeq-pebble"
	}
	if err := os.MkdirAll(pc.Path, 0o755); err != nil {
		return nil, fmt.Errorf("ensure pebble dir %s: %w", pc.Path, err)
	}

	// Phase 8: shard the Pebble write side per-shard so commit pipelines
	// + compaction run in parallel within a single process. NumShards=0
	// or 1 keeps the historical single-DB layout (and skips the subdir
	// indirection). N>1 opens Path/shard<i>/ for each shard.
	numShards := pc.NumShards
	if numShards <= 0 {
		numShards = 1
	}
	dbs := make([]*pebblerepo.DB, 0, numShards)
	openShard := func(idx int) error {
		shardPath := pc.Path
		if numShards > 1 {
			shardPath = fmt.Sprintf("%s/shard%d", pc.Path, idx)
			if err := os.MkdirAll(shardPath, 0o755); err != nil {
				return fmt.Errorf("ensure pebble shard dir %s: %w", shardPath, err)
			}
		}
		shardDB, err := pebblerepo.Open(pebblerepo.Options{Path: shardPath, FsyncOnCommit: pc.FsyncOnCommit})
		if err != nil {
			return fmt.Errorf("open pebble shard %d: %w", idx, err)
		}
		dbs = append(dbs, shardDB)
		return nil
	}
	for i := range numShards {
		if err := openShard(i); err != nil {
			for _, d := range dbs {
				_ = d.Close()
			}
			return nil, err
		}
	}
	// db remains as the first shard for any code path that still expects
	// a single *DB (cluster.Server, subscription repo). Subscription
	// data stays unsharded for now; it's never the bottleneck.
	db := dbs[0]

	// background goroutines (reaper, gossiper, subscription cleanup) share a
	// cancellable context that the Application's shutdown hook cancels
	// BEFORE closing the Pebble DB — otherwise the reaper wakes on its
	// next tick against a closed DB and panics.
	bgCtx, bgCancel := context.WithCancel(context.Background())

	taskShards := make([]*pebblerepo.TaskRepository, len(dbs))
	resultShards := make([]*pebblerepo.ResultRepository, len(dbs))
	for i, d := range dbs {
		taskShards[i] = pebblerepo.NewTaskRepository(d, loc, cfg.BackoffPolicy, cfg.BackoffBaseSeconds, cfg.BackoffMaxSeconds)
		resultShards[i] = pebblerepo.NewResultRepository(d, loc)
	}
	localTaskRepo := taskShards[0]
	localResultRepo := resultShards[0]
	subRepo := pebblerepo.NewSubscriptionRepository(db, loc)

	// In cluster mode, wrap the local Pebble repos with routers that:
	//   - hash-route ID-aware operations to the owning node via gRPC
	//   - scatter-gather Claim across every node
	// The service layer above takes plain repository interfaces, so it
	// doesn't observe whether it's holding the local Pebble repo or the
	// router. The internal gRPC server delegates to the SAME local repo,
	// so requests that hash back to this node short-circuit the network.
	var (
		taskRepo   repository.TaskRepository   = localTaskRepo
		resultRepo repository.ResultRepository = localResultRepo
		grpcSrv    *grpc.Server
		grpcLis    net.Listener
		clientPool *cluster.ClientPool
	)
	if numShards > 1 {
		// Cluster mode + intra-process sharding is not supported in this
		// first cut — cluster.Server expects a single concrete
		// *TaskRepository, and a sharded wrapper would need its own
		// cluster bridge. Single-node sharded is fine; multi-node
		// sharded is a follow-up.
		if cfg.Cluster.Enabled {
			for _, d := range dbs {
				_ = d.Close()
			}
			return nil, fmt.Errorf("pebble: cluster mode + intra-process shards not supported (pick one)")
		}
		taskRepo = pebblerepo.NewShardedTaskRepository(taskShards)
		resultRepo = pebblerepo.NewShardedResultRepository(resultShards)
		logger.Info("pebble shards enabled", "shards", numShards)
	}
	if cfg.Cluster.Enabled {
		if err := validateClusterConfig(cfg.Cluster); err != nil {
			bgCancel()
			_ = db.Close()
			return nil, err
		}
		nodes := make([]cluster.Node, 0, len(cfg.Cluster.Nodes))
		for _, n := range cfg.Cluster.Nodes {
			nodes = append(nodes, cluster.Node{ID: n.ID, GRPCAddr: n.GRPCAddr})
		}
		ring := cluster.NewLocalRing(cluster.NewRing(nodes), cfg.Cluster.SelfID)
		clientPool = cluster.NewClientPool()

		// Bloom of locally-stored task IDs; gossiped to peers so they can
		// short-circuit ID-routed lookups for ids that definitely aren't
		// on this node. 1M expected items at 0.1% FP rate sizes the
		// filter at ~1.7 MiB — cheap to ship across the wire on the 1s
		// gossip cadence.
		localBloom := cluster.NewBloom(1_000_000, 0.001)
		bloomCache := cluster.NewBloomCache(1_000_000, 0.001)

		// Start the internal gRPC server. Local repos serve every RPC;
		// the router on this node will short-circuit calls that hash to
		// SelfID and only talk gRPC for peers.
		var err error
		grpcLis, err = net.Listen("tcp", cfg.Cluster.GRPCAddr)
		if err != nil {
			bgCancel()
			_ = db.Close()
			return nil, fmt.Errorf("cluster gRPC listen %s: %w", cfg.Cluster.GRPCAddr, err)
		}
		grpcSrv = grpc.NewServer()
		clusterpb.RegisterTaskNodeServer(grpcSrv, &cluster.Server{
			NodeID:     cfg.Cluster.SelfID,
			Tasks:      localTaskRepo,
			Results:    localResultRepo,
			LocalBloom: localBloom,
		})
		go func() {
			if err := grpcSrv.Serve(grpcLis); err != nil && err != grpc.ErrServerStopped {
				logger.Error("cluster gRPC server stopped", "err", err)
			}
		}()
		logger.Info("cluster mode enabled",
			"selfID", cfg.Cluster.SelfID,
			"grpcAddr", cfg.Cluster.GRPCAddr,
			"peers", len(nodes)-1)

		taskRepo = cluster.NewTaskRouter(localTaskRepo, ring, clientPool).
			WithBloomCache(bloomCache).
			WithLocalBloom(localBloom)
		resultRepo = cluster.NewResultRouter(localResultRepo, ring, clientPool).
			WithBloomCache(bloomCache)

		// Gossip peer blooms in the background. 1s default cadence; tests
		// can force a poll via Gossiper but this is enough for production.
		gossiper := cluster.NewGossiper(ring, clientPool, bloomCache, time.Second, logger)
		gossiper.Start(bgCtx)
	}

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

	// Background reaper: enforces lease expiry + TTL cleanup, which Redis
	// gives us via key TTL. Phase 8: one reaper per Pebble shard so the
	// sweeps run in parallel and the per-shard commit pipelines stay
	// independent.
	pebblerepo.StartReapersForShards(bgCtx, dbs, loc, logger, pebblerepo.ReaperOptions{
		BackoffPolicy:      cfg.BackoffPolicy,
		BackoffBaseSeconds: cfg.BackoffBaseSeconds,
		BackoffMaxSeconds:  cfg.BackoffMaxSeconds,
		MaxAttemptsDefault: cfg.MaxAttemptsDefault,
		DLQCallback: func(ctx context.Context, t domain.Task, rec domain.ResultRecord) {
			if resultCallback != nil {
				resultCallback.Send(ctx, t, rec)
			}
		},
	})

	scheduler := services.NewSchedulerService(
		taskRepo,
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

	// Result service & uploader mirror the redis path verbatim.
	uploader := providers.NewLocalUploader(cfg.LocalArtifactsDir)
	results := services.NewResultsService(resultRepo, uploader, resultCallback, logger, time.Now, loc)

	engine := gin.New()
	engine.Use(gin.Recovery(), middleware.RequestIDMiddleware())
	if cfg.TracingEnabled {
		engine.Use(middleware.TracingMiddleware(cfg.TracingServiceName))
	}
	engine.Use(middleware.LoggerMiddleware(logger))

	// Subscription cleanup goroutine — same cadence as the redis path.
	go cleanup.Start(bgCtx)

	app := &Application{
		Config:      cfg,
		Engine:      engine,
		Scheduler:   scheduler,
		Results:     results,
		Subs:        subs,
		Logger:      logger,
		TZ:          loc,
		RateLimiter: limiter,
	}

	cleanupStartupFailure := func() {
		bgCancel()
		if grpcSrv != nil {
			grpcSrv.Stop()
		}
		if grpcLis != nil {
			_ = grpcLis.Close()
		}
		if clientPool != nil {
			_ = clientPool.Close()
		}
		for _, d := range dbs {
			if cerr := d.Close(); cerr != nil {
				logger.Warn("pebble close after startup failure", "err", cerr)
			}
		}
	}

	for _, o := range opts {
		if err := o(app); err != nil {
			cleanupStartupFailure()
			return nil, err
		}
	}
	if app.ProducerValidator == nil && cfg.ProducerAuthProvider != "" {
		v, err := auth.NewValidator(auth.ProviderConfig{Type: cfg.ProducerAuthProvider, Config: cfg.ProducerAuthConfig})
		if err != nil {
			cleanupStartupFailure()
			return nil, err
		}
		app.ProducerValidator = v
	}
	if app.WorkerValidator == nil && cfg.WorkerAuthProvider != "" {
		v, err := auth.NewValidator(auth.ProviderConfig{Type: cfg.WorkerAuthProvider, Config: cfg.WorkerAuthConfig})
		if err != nil {
			cleanupStartupFailure()
			return nil, err
		}
		app.WorkerValidator = v
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
		cleanupStartupFailure()
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
		cleanupStartupFailure()
		return nil, err
	}

	app.TracingShutdown = func(ctx context.Context) error {
		bgCancel()
		stopGRPCServer(ctx, &grpcServerHandle{srv: grpcSrv, lis: grpcLis})
		stopGRPCServer(ctx, workerStream)
		stopGRPCServer(ctx, producerStream)
		if clientPool != nil {
			_ = clientPool.Close()
		}
		for _, d := range dbs {
			if err := d.Close(); err != nil {
				logger.Warn("pebble close", "err", err)
			}
		}
		if tracingShutdown == nil {
			return nil
		}
		return tracingShutdown(ctx)
	}

	return app, nil
}
