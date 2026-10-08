package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"net"

	"google.golang.org/grpc"

	schedulesapp "github.com/osvaldoandrade/codeq/internal/application/schedules"
	topicsapp "github.com/osvaldoandrade/codeq/internal/application/topics"
	"github.com/osvaldoandrade/codeq/internal/cluster"
	"github.com/osvaldoandrade/codeq/internal/cluster/clusterpb"
	"github.com/osvaldoandrade/codeq/internal/leaderforward"
	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/providers"
	raftpkg "github.com/osvaldoandrade/codeq/internal/raft"
	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/internal/repository"
	pebblerepo "github.com/osvaldoandrade/codeq/internal/repository/pebble"
	"github.com/osvaldoandrade/codeq/internal/services"
	topicpebble "github.com/osvaldoandrade/codeq/internal/storage/adapter/pebble"
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

// pebbleRuntime is the composition root for one process. Each method
// owns one setup step so no function carries the whole startup graph.
type pebbleRuntime struct {
	cfg          *config.Config
	pc           pebbleConfig
	loc          *time.Location
	logger       *slog.Logger
	webhook      *http.Client
	opts         []ApplicationOption
	dbs          []*pebblerepo.DB
	raftNodes    []*raftpkg.DB
	mux          *raftpkg.MuxAcceptor
	bgCtx        context.Context
	bgCancel     context.CancelFunc
	taskShards   []*pebblerepo.TaskRepository
	resultShards []*pebblerepo.ResultRepository
	taskRepo     repository.TaskRepository
	resultRepo   repository.ResultRepository
	subRepo      repository.SubscriptionRepository
	subs         services.SubscriptionService
	grpcSrv      *grpc.Server
	grpcLis      net.Listener
	pool         *cluster.ClientPool
	limiter      *ratelimit.InMemoryLimiter
}

// newPebbleApplication constructs the Application stack on embedded Pebble.
// Rate limiting is process-local (one bucket map per process).
func newPebbleApplication(cfg *config.Config, opts ...ApplicationOption) (*Application, error) {
	rt := &pebbleRuntime{
		cfg:     cfg,
		loc:     locationOrUTC(cfg.Timezone),
		logger:  slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
		webhook: &http.Client{Timeout: 15 * time.Second},
		opts:    opts,
	}
	pc, err := parsePebbleConfig(cfg)
	if err != nil {
		return nil, err
	}
	rt.pc = pc
	rt.dbs, err = openPebbleShards(pc)
	if err != nil {
		return nil, err
	}
	rt.bgCtx, rt.bgCancel = context.WithCancel(context.Background())
	if err = rt.openRaft(); err != nil {
		rt.closeAfterFailure()
		return nil, err
	}
	if err = rt.routeRepositories(); err != nil {
		rt.closeAfterFailure()
		return nil, err
	}
	return rt.finish()
}

func locationOrUTC(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil || loc == nil {
		return time.UTC
	}
	return loc
}

func parsePebbleConfig(cfg *config.Config) (pebbleConfig, error) {
	var pc pebbleConfig
	if len(cfg.PersistenceConfig) > 0 {
		if err := json.Unmarshal(cfg.PersistenceConfig, &pc); err != nil {
			return pebbleConfig{}, fmt.Errorf("parse pebble PersistenceConfig: %w", err)
		}
	}
	if pc.Path == "" {
		pc.Path = "./codeq-pebble"
	}
	if err := os.MkdirAll(pc.Path, 0o700); err != nil {
		return pebbleConfig{}, fmt.Errorf("ensure pebble dir %s: %w", pc.Path, err)
	}
	if pc.NumShards <= 0 {
		pc.NumShards = 1
	}
	return pc, nil
}

func openPebbleShards(pc pebbleConfig) ([]*pebblerepo.DB, error) {
	dbs := make([]*pebblerepo.DB, 0, pc.NumShards)
	for i := range pc.NumShards {
		db, err := openOnePebbleShard(pc, i)
		if err != nil {
			closePebbleDBs(dbs)
			return nil, err
		}
		dbs = append(dbs, db)
	}
	return dbs, nil
}

func openOnePebbleShard(pc pebbleConfig, idx int) (*pebblerepo.DB, error) {
	shardPath := pc.Path
	if pc.NumShards > 1 {
		shardPath = fmt.Sprintf("%s/shard%d", pc.Path, idx)
		if err := os.MkdirAll(shardPath, 0o700); err != nil {
			return nil, fmt.Errorf("ensure pebble shard dir %s: %w", shardPath, err)
		}
	}
	db, err := pebblerepo.Open(pebblerepo.Options{Path: shardPath, FsyncOnCommit: pc.FsyncOnCommit})
	if err != nil {
		return nil, fmt.Errorf("open pebble shard %d: %w", idx, err)
	}
	return db, nil
}

func closePebbleDBs(dbs []*pebblerepo.DB) {
	for _, db := range dbs {
		_ = db.Close()
	}
}

func (rt *pebbleRuntime) openRaft() error {
	rt.raftNodes = make([]*raftpkg.DB, len(rt.dbs))
	if !rt.cfg.Raft.Enabled {
		return nil
	}
	mux, err := openMuxAcceptor(rt.cfg)
	if err != nil {
		return err
	}
	rt.mux = mux
	for i, shardDB := range rt.dbs {
		rdb, err := openRaftShard(rt.bgCtx, rt.cfg, rt.pc, mux, i, shardDB)
		if err != nil {
			return err
		}
		rt.raftNodes[i] = rdb
	}
	rt.logger.Info("raft replication enabled",
		"selfID", rt.cfg.Raft.SelfID,
		"baseBindAddr", rt.cfg.Raft.BindAddr,
		"peers", len(rt.cfg.Raft.Peers),
		"shards", len(rt.dbs))
	return nil
}

func openMuxAcceptor(cfg *config.Config) (*raftpkg.MuxAcceptor, error) {
	if !cfg.Raft.MuxEnabled {
		return nil, nil
	}
	acc, err := raftpkg.NewMuxAcceptor(cfg.Raft.BindAddr, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft mux acceptor: %w", err)
	}
	return acc, nil
}

func openRaftShard(ctx context.Context, cfg *config.Config, pc pebbleConfig, mux *raftpkg.MuxAcceptor, idx int, shardDB *pebblerepo.DB) (*raftpkg.DB, error) {
	shardPath := pc.Path
	if pc.NumShards > 1 {
		shardPath = fmt.Sprintf("%s/shard%d", pc.Path, idx)
	}
	raftCfg := raftpkg.Config{
		Path:          shardPath,
		SelfID:        cfg.Raft.SelfID,
		Bootstrap:     cfg.Raft.Bootstrap,
		PeerHTTPAddrs: cfg.Raft.PeerHTTPAddrs,
		HeartbeatMS:   cfg.Raft.HeartbeatMS,
		ElectionMS:    cfg.Raft.ElectionMS,
		LeaderLeaseMS: cfg.Raft.LeaderLeaseMS,
		CommitMS:      cfg.Raft.CommitMS,
	}
	if err := applyShardTransport(cfg, mux, idx, &raftCfg); err != nil {
		return nil, err
	}
	if cfg.Raft.ApplyTimeoutSeconds > 0 {
		raftCfg.ApplyTimeout = time.Duration(cfg.Raft.ApplyTimeoutSeconds) * time.Second
	}
	rdb, err := raftpkg.OpenWithPebble(ctx, raftCfg, shardDB.Raw())
	if err != nil {
		return nil, fmt.Errorf("raft open shard %d: %w", idx, err)
	}
	shardDB.AttachReplicator(rdb)
	return rdb, nil
}

func applyShardTransport(cfg *config.Config, mux *raftpkg.MuxAcceptor, idx int, raftCfg *raftpkg.Config) error {
	if mux != nil {
		return applyMuxShard(cfg, mux, idx, raftCfg)
	}
	return applyOffsetShard(cfg, idx, raftCfg)
}

func applyMuxShard(cfg *config.Config, mux *raftpkg.MuxAcceptor, idx int, raftCfg *raftpkg.Config) error {
	// Shard index is a configured count, not an untrusted integer.
	sl, err := mux.RegisterGroup(uint32(idx)) // #nosec G115 -- idx is a small non-negative shard number
	if err != nil {
		return fmt.Errorf("raft mux register shard %d: %w", idx, err)
	}
	raftCfg.StreamLayer = sl
	raftCfg.BindAddr = mux.Addr().String()
	raftCfg.PeerAddrs = cfg.Raft.Peers
	return nil
}

func applyOffsetShard(cfg *config.Config, idx int, raftCfg *raftpkg.Config) error {
	addr, err := bindAddrForShard(cfg.Raft.BindAddr, idx)
	if err != nil {
		return fmt.Errorf("raft bind addr shard %d: %w", idx, err)
	}
	peers, err := peersForShard(cfg.Raft.Peers, idx)
	if err != nil {
		return fmt.Errorf("raft peers shard %d: %w", idx, err)
	}
	raftCfg.BindAddr = addr
	raftCfg.PeerAddrs = peers
	return nil
}

func (rt *pebbleRuntime) routeRepositories() error {
	rt.taskShards = make([]*pebblerepo.TaskRepository, len(rt.dbs))
	rt.resultShards = make([]*pebblerepo.ResultRepository, len(rt.dbs))
	for i, db := range rt.dbs {
		rt.taskShards[i] = pebblerepo.NewTaskRepository(db, rt.loc, rt.cfg.BackoffPolicy, rt.cfg.BackoffBaseSeconds, rt.cfg.BackoffMaxSeconds)
		rt.resultShards[i] = pebblerepo.NewResultRepository(db, rt.loc)
	}
	rt.taskRepo = rt.taskShards[0]
	rt.resultRepo = rt.resultShards[0]
	rt.subRepo = pebblerepo.NewSubscriptionRepository(rt.dbs[0], rt.loc)
	if rt.pc.NumShards > 1 {
		if rt.cfg.Cluster.Enabled {
			return fmt.Errorf("pebble: cluster mode + intra-process shards not supported (pick one)")
		}
		rt.taskRepo = pebblerepo.NewShardedTaskRepository(rt.taskShards)
		rt.resultRepo = pebblerepo.NewShardedResultRepository(rt.resultShards)
		rt.logger.Info("pebble shards enabled", "shards", rt.pc.NumShards)
		return nil
	}
	if !rt.cfg.Cluster.Enabled {
		return nil
	}
	return rt.startCluster()
}

func (rt *pebbleRuntime) startCluster() error {
	if err := validateClusterConfig(rt.cfg.Cluster); err != nil {
		return err
	}
	nodes := make([]cluster.Node, 0, len(rt.cfg.Cluster.Nodes))
	for _, n := range rt.cfg.Cluster.Nodes {
		nodes = append(nodes, cluster.Node{ID: n.ID, GRPCAddr: n.GRPCAddr})
	}
	ring := cluster.NewLocalRing(cluster.NewRing(nodes), rt.cfg.Cluster.SelfID)
	rt.pool = cluster.NewClientPool()
	localBloom := cluster.NewBloom(1_000_000, 0.001)
	bloomCache := cluster.NewBloomCache(1_000_000, 0.001)
	lis, err := net.Listen("tcp", rt.cfg.Cluster.GRPCAddr)
	if err != nil {
		return fmt.Errorf("cluster gRPC listen %s: %w", rt.cfg.Cluster.GRPCAddr, err)
	}
	rt.grpcLis = lis
	rt.grpcSrv = grpc.NewServer()
	clusterpb.RegisterTaskNodeServer(rt.grpcSrv, &cluster.Server{
		NodeID:     rt.cfg.Cluster.SelfID,
		Tasks:      rt.taskShards[0],
		Results:    rt.resultShards[0],
		LocalBloom: localBloom,
	})
	go serveClusterGRPC(rt.grpcSrv, rt.grpcLis, rt.logger)
	rt.logger.Info("cluster mode enabled",
		"selfID", rt.cfg.Cluster.SelfID,
		"grpcAddr", rt.cfg.Cluster.GRPCAddr,
		"peers", len(nodes)-1)
	rt.taskRepo = cluster.NewTaskRouter(rt.taskShards[0], ring, rt.pool).
		WithBloomCache(bloomCache).
		WithLocalBloom(localBloom)
	rt.resultRepo = cluster.NewResultRouter(rt.resultShards[0], ring, rt.pool).
		WithBloomCache(bloomCache)
	cluster.NewGossiper(ring, rt.pool, bloomCache, time.Second, rt.logger).Start(rt.bgCtx)
	return nil
}

func serveClusterGRPC(srv *grpc.Server, lis net.Listener, logger *slog.Logger) {
	if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		logger.Error("cluster gRPC server stopped", "err", err)
	}
}

func (rt *pebbleRuntime) finish() (*Application, error) {
	rt.limiter = ratelimit.NewInMemoryLimiter()
	notifier, cleanup, callback := rt.notificationServices()
	rt.startReapers(callback)
	scheduler := services.NewSchedulerService(
		rt.taskRepo, notifier, callback, rt.loc, time.Now,
		rt.cfg.DefaultLeaseSeconds, rt.cfg.RequeueInspectLimit, rt.cfg.MaxAttemptsDefault,
		rt.cfg.BackoffPolicy, rt.cfg.BackoffBaseSeconds, rt.cfg.BackoffMaxSeconds,
	)
	results := services.NewResultsService(
		rt.resultRepo, providers.NewLocalUploader(rt.cfg.LocalArtifactsDir), callback, rt.logger, time.Now, rt.loc,
	)
	go cleanup.Start(rt.bgCtx)
	app := rt.newApplication(rt.httpEngine(), scheduler, results)
	app.Schedules = rt.startSchedules(notifier, callback)
	if err := rt.applyOptions(app); err != nil {
		return nil, err
	}
	if err := rt.installValidators(app); err != nil {
		return nil, err
	}
	return rt.openStreams(app, scheduler, results)
}

func (rt *pebbleRuntime) notificationServices() (services.NotifierService, services.SubscriptionCleanupService, services.ResultCallbackService) {
	rt.subs = services.NewSubscriptionService(rt.subRepo)
	bucket := ratelimit.Bucket(rt.cfg.RateLimit.Webhook)
	notifier := services.NewNotifierService(rt.subRepo, rt.logger, rt.cfg.WebhookHmacSecret, rt.cfg.SubscriptionMinIntervalSeconds, rt.limiter, bucket, rt.webhook)
	cleanup := services.NewSubscriptionCleanupService(rt.subRepo, rt.logger, rt.cfg.SubscriptionCleanupIntervalSeconds)
	callback := services.NewResultCallbackService(
		rt.logger, rt.cfg.WebhookHmacSecret, rt.cfg.ResultWebhookMaxAttempts,
		rt.cfg.ResultWebhookBaseBackoffSeconds, rt.cfg.ResultWebhookMaxBackoffSeconds,
		rt.limiter, bucket, rt.webhook,
	)
	return notifier, cleanup, callback
}

func (rt *pebbleRuntime) startReapers(callback services.ResultCallbackService) {
	opts := pebblerepo.ReaperOptions{
		BackoffPolicy:      rt.cfg.BackoffPolicy,
		BackoffBaseSeconds: rt.cfg.BackoffBaseSeconds,
		BackoffMaxSeconds:  rt.cfg.BackoffMaxSeconds,
		MaxAttemptsDefault: rt.cfg.MaxAttemptsDefault,
		DLQCallback: func(ctx context.Context, task domain.Task, rec domain.ResultRecord) {
			if callback != nil {
				callback.Send(ctx, task, rec)
			}
		},
	}
	for i, shardDB := range rt.dbs {
		shardOpts := opts
		repo := rt.taskShards[i]
		shardOpts.OnDelayed = repo.NoteDelayed
		if rt.cfg.Raft.Enabled && rt.raftNodes[i] != nil {
			ref := rt.raftNodes[i]
			shardOpts.LeaderGate = ref.IsLeader
		}
		pebblerepo.NewReaper(shardDB, rt.loc, rt.logger, shardOpts).Start(rt.bgCtx)
	}
}

// startSchedules wires the recurring schedule catalog on the first shard
// and, when this deployment can fire each slot exactly once, starts the
// runner gated on that shard's leadership (ADR 0006). Scheduled tasks are
// created on the first shard too, so the runner never depends on the
// leaders of the other shards.
func (rt *pebbleRuntime) startSchedules(notifier services.NotifierService, callback services.ResultCallbackService) *schedulesapp.Service {
	if rt.cfg.Cluster.Enabled {
		return schedulesapp.NewUnavailableService("cluster mode keeps one catalog per node; use Raft for replicated schedules")
	}
	if rt.cfg.Raft.Enabled && strings.TrimSpace(rt.cfg.Raft.ScheduleCatalogProtocol) != "v1" {
		return schedulesapp.NewUnavailableService("raft.scheduleCatalogProtocol=v1 is required for replicated schedules")
	}
	store := topicpebble.NewScheduleStore(rt.dbs[0])
	var tasks repository.TaskRepository = rt.taskShards[0]
	if sharded, ok := rt.taskRepo.(*pebblerepo.ShardedTaskRepository); ok {
		tasks = sharded.OnShard(0)
	}
	creator := services.NewSchedulerService(
		tasks, notifier, callback, rt.loc, time.Now,
		rt.cfg.DefaultLeaseSeconds, rt.cfg.RequeueInspectLimit, rt.cfg.MaxAttemptsDefault,
		rt.cfg.BackoffPolicy, rt.cfg.BackoffBaseSeconds, rt.cfg.BackoffMaxSeconds,
	)
	opts := schedulesapp.RunnerOptions{Logger: rt.logger}
	if rt.cfg.Raft.Enabled && len(rt.raftNodes) > 0 && rt.raftNodes[0] != nil {
		opts.LeaderGate = rt.raftNodes[0].IsLeader
	}
	schedulesapp.NewRunner(store, creator, schedulesapp.ParseCron, opts).Start(rt.bgCtx)
	return schedulesapp.NewService(store, schedulesapp.ParseCron, time.Now)
}

func (rt *pebbleRuntime) httpEngine() *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery(), middleware.RequestIDMiddleware())
	if rt.cfg.TracingEnabled {
		engine.Use(middleware.TracingMiddleware(rt.cfg.TracingServiceName))
	}
	engine.Use(middleware.LoggerMiddleware(rt.logger))
	return engine
}

func (rt *pebbleRuntime) newApplication(engine *gin.Engine, scheduler services.SchedulerService, results services.ResultsService) *Application {
	topicService := topicsapp.NewService(topicpebble.NewTopicStore(rt.dbs[0]), time.Now)
	if rt.cfg.Raft.Enabled && strings.TrimSpace(rt.cfg.Raft.TopicCatalogProtocol) != "v1" {
		topicService = topicsapp.NewUnavailableService(
			"raft.topicCatalogProtocol=v1 is required for replicated topic catalog writes",
		)
	}
	app := &Application{
		Config:      rt.cfg,
		Engine:      engine,
		Scheduler:   scheduler,
		Results:     results,
		Subs:        rt.subs,
		Topics:      topicService,
		Logger:      rt.logger,
		TZ:          rt.loc,
		RateLimiter: rt.limiter,
	}
	rt.attachRaft(app)
	return app
}

func (rt *pebbleRuntime) attachRaft(app *Application) {
	if !rt.cfg.Raft.Enabled {
		return
	}
	app.RaftGroups = make([]RaftGroupStatus, 0, len(rt.raftNodes))
	for _, node := range rt.raftNodes {
		if node != nil {
			app.RaftGroups = append(app.RaftGroups, node)
		}
	}
	groups := make([]leaderforward.Leadership, len(rt.dbs))
	for i, db := range rt.dbs {
		groups[i] = db
	}
	app.LeaderForward = leaderforward.New(leaderforward.Config{
		PeerHTTPAddrs: rt.cfg.Raft.PeerHTTPAddrs,
		SelfID:        rt.cfg.Raft.SelfID,
		Groups:        groups,
		Logger:        rt.logger,
	})
}

func (rt *pebbleRuntime) applyOptions(app *Application) error {
	for _, opt := range rt.opts {
		if err := opt(app); err != nil {
			rt.closeAfterFailure()
			return err
		}
	}
	return nil
}

func (rt *pebbleRuntime) installValidators(app *Application) error {
	if app.ProducerValidator == nil && rt.cfg.ProducerAuthProvider != "" {
		v, err := auth.NewValidator(auth.ProviderConfig{Type: rt.cfg.ProducerAuthProvider, Config: rt.cfg.ProducerAuthConfig})
		if err != nil {
			rt.closeAfterFailure()
			return err
		}
		app.ProducerValidator = v
	}
	if app.WorkerValidator == nil && rt.cfg.WorkerAuthProvider != "" {
		v, err := auth.NewValidator(auth.ProviderConfig{Type: rt.cfg.WorkerAuthProvider, Config: rt.cfg.WorkerAuthConfig})
		if err != nil {
			rt.closeAfterFailure()
			return err
		}
		app.WorkerValidator = v
	}
	return nil
}

func (rt *pebbleRuntime) openStreams(app *Application, scheduler services.SchedulerService, results services.ResultsService) (*Application, error) {
	workerStream, err := startWorkerStreamServer(rt.cfg, scheduler, results, app.WorkerValidator, app.ProducerValidator, rt.logger)
	if err != nil {
		rt.closeAfterFailure()
		return nil, err
	}
	producerStream, err := startProducerStreamServer(rt.cfg, scheduler, app.ProducerValidator, rt.logger)
	if err != nil {
		stopGRPCServer(context.Background(), workerStream)
		rt.closeAfterFailure()
		return nil, err
	}
	app.TracingShutdown = func(ctx context.Context) error {
		return rt.shutdown(ctx, workerStream, producerStream)
	}
	return app, nil
}

func (rt *pebbleRuntime) shutdown(ctx context.Context, worker, producer *grpcServerHandle) error {
	_ = rt.limiter.Close()
	rt.bgCancel()
	stopGRPCServer(ctx, &grpcServerHandle{srv: rt.grpcSrv, lis: rt.grpcLis})
	stopGRPCServer(ctx, worker)
	stopGRPCServer(ctx, producer)
	if rt.pool != nil {
		_ = rt.pool.Close()
	}
	rt.closeRaft("raft close")
	rt.closeMux("mux acceptor close")
	rt.closeDBs("pebble close")
	return nil
}

func (rt *pebbleRuntime) closeAfterFailure() {
	if rt.limiter != nil {
		_ = rt.limiter.Close()
	}
	if rt.bgCancel != nil {
		rt.bgCancel()
	}
	if rt.grpcSrv != nil {
		rt.grpcSrv.Stop()
	}
	if rt.grpcLis != nil {
		_ = rt.grpcLis.Close()
	}
	if rt.pool != nil {
		_ = rt.pool.Close()
	}
	rt.closeRaft("raft close after startup failure")
	if rt.mux != nil {
		_ = rt.mux.Close()
	}
	rt.closeDBs("pebble close after startup failure")
}

func (rt *pebbleRuntime) closeRaft(msg string) {
	for _, node := range rt.raftNodes {
		if node == nil {
			continue
		}
		if err := node.Close(); err != nil {
			rt.logger.Warn(msg, "err", err)
		}
	}
}

func (rt *pebbleRuntime) closeMux(msg string) {
	if rt.mux == nil {
		return
	}
	if err := rt.mux.Close(); err != nil {
		rt.logger.Warn(msg, "err", err)
	}
}

func (rt *pebbleRuntime) closeDBs(msg string) {
	for _, db := range rt.dbs {
		if err := db.Close(); err != nil {
			rt.logger.Warn(msg, "err", err)
		}
	}
}
