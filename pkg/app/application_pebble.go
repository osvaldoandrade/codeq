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

	db, err := pebblerepo.Open(pebblerepo.Options{Path: pc.Path, FsyncOnCommit: pc.FsyncOnCommit})
	if err != nil {
		return nil, fmt.Errorf("open pebble: %w", err)
	}

	localTaskRepo := pebblerepo.NewTaskRepository(db, loc, cfg.BackoffPolicy, cfg.BackoffBaseSeconds, cfg.BackoffMaxSeconds)
	localResultRepo := pebblerepo.NewResultRepository(db, loc)
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
	if cfg.Cluster.Enabled {
		if err := validateClusterConfig(cfg.Cluster); err != nil {
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
		grpcLis, err = net.Listen("tcp", cfg.Cluster.GRPCAddr)
		if err != nil {
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
		resultRepo = cluster.NewResultRouter(localResultRepo, ring, clientPool)

		// Gossip peer blooms in the background. 1s default cadence; tests
		// can force a poll via Gossiper but this is enough for production.
		gossiper := cluster.NewGossiper(ring, clientPool, bloomCache, time.Second, logger)
		gossiper.Start(context.Background())
	}

	// Background reaper: enforces lease expiry + TTL cleanup, which Redis
	// gives us via key TTL.
	reaper := pebblerepo.NewReaper(db, loc, logger, pebblerepo.ReaperOptions{
		BackoffPolicy:      cfg.BackoffPolicy,
		BackoffBaseSeconds: cfg.BackoffBaseSeconds,
		BackoffMaxSeconds:  cfg.BackoffMaxSeconds,
		MaxAttemptsDefault: cfg.MaxAttemptsDefault,
	})
	reaper.Start(context.Background())

	subs := services.NewSubscriptionService(subRepo)
	notifier := services.NewNotifierService(subRepo, logger, cfg.WebhookHmacSecret, cfg.SubscriptionMinIntervalSeconds, limiter, ratelimit.Bucket(cfg.RateLimit.Webhook), webhookClient)
	cleanup := services.NewSubscriptionCleanupService(subRepo, logger, cfg.SubscriptionCleanupIntervalSeconds)
	scheduler := services.NewSchedulerService(
		taskRepo,
		notifier,
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
	results := services.NewResultsService(resultRepo, uploader, resultCallback, logger, time.Now, loc)

	engine := gin.New()
	engine.Use(gin.Recovery(), middleware.RequestIDMiddleware())
	if cfg.TracingEnabled {
		engine.Use(middleware.TracingMiddleware(cfg.TracingServiceName))
	}
	engine.Use(middleware.LoggerMiddleware(logger))

	// Subscription cleanup goroutine — same cadence as the redis path.
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
			if grpcSrv != nil {
				// GracefulStop drains in-flight RPCs first, then exits.
				done := make(chan struct{})
				go func() { grpcSrv.GracefulStop(); close(done) }()
				select {
				case <-done:
				case <-ctx.Done():
					grpcSrv.Stop()
				}
			}
			if clientPool != nil {
				_ = clientPool.Close()
			}
			if grpcLis != nil {
				_ = grpcLis.Close()
			}
			if err := db.Close(); err != nil {
				logger.Warn("pebble close", "err", err)
			}
			if tracingShutdown == nil {
				return nil
			}
			return tracingShutdown(ctx)
		},
	}

	for _, o := range opts {
		if err := o(app); err != nil {
			return nil, err
		}
	}
	if app.ProducerValidator == nil && cfg.ProducerAuthProvider != "" {
		v, err := auth.NewValidator(auth.ProviderConfig{Type: cfg.ProducerAuthProvider, Config: cfg.ProducerAuthConfig})
		if err != nil {
			return nil, err
		}
		app.ProducerValidator = v
	}
	if app.WorkerValidator == nil && cfg.WorkerAuthProvider != "" {
		v, err := auth.NewValidator(auth.ProviderConfig{Type: cfg.WorkerAuthProvider, Config: cfg.WorkerAuthConfig})
		if err != nil {
			return nil, err
		}
		app.WorkerValidator = v
	}
	return app, nil
}
