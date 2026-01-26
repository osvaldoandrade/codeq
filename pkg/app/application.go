package app

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/providers"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type Application struct {
	Config    *config.Config
	Engine    *gin.Engine
	Scheduler services.SchedulerService
	Results   services.ResultsService
	Subs      services.SubscriptionService
	Logger    *slog.Logger
	TZ        *time.Location
}

func NewApplication(cfg *config.Config) *Application {
	redisClient := providers.NewRedisProvider(cfg.RedisAddr)

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

	repo := repository.NewTaskRepository(redisClient, loc, cfg.BackoffPolicy, cfg.BackoffBaseSeconds, cfg.BackoffMaxSeconds)
	subRepo := repository.NewSubscriptionRepository(redisClient, loc)
	subs := services.NewSubscriptionService(subRepo)
	notifier := services.NewNotifierService(subRepo, logger, cfg.WebhookHmacSecret, cfg.SubscriptionMinIntervalSeconds)
	cleanup := services.NewSubscriptionCleanupService(subRepo, logger, cfg.SubscriptionCleanupIntervalSeconds)
	scheduler := services.NewSchedulerService(
		repo,
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
	resultRepo := repository.NewResultRepository(redisClient, loc)
	uploader := providers.NewLocalUploader(cfg.LocalArtifactsDir)
	resultCallback := services.NewResultCallbackService(
		logger,
		cfg.WebhookHmacSecret,
		cfg.ResultWebhookMaxAttempts,
		cfg.ResultWebhookBaseBackoffSeconds,
		cfg.ResultWebhookMaxBackoffSeconds,
	)
	results := services.NewResultsService(resultRepo, uploader, resultCallback, logger, time.Now, loc)

	engine := gin.New()
	engine.Use(gin.Recovery(), middleware.RequestIDMiddleware(), middleware.LoggerMiddleware(logger))

	go cleanup.Start(context.Background())

	return &Application{
		Config:    cfg,
		Engine:    engine,
		Scheduler: scheduler,
		Results:   results,
		Subs:      subs,
		Logger:    logger,
		TZ:        loc,
	}
}
