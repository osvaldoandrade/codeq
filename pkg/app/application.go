package app

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/providers"
	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

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

func NewApplication(cfg *config.Config, opts ...ApplicationOption) (*Application, error) {
	redisClient := providers.NewRedisProvider(cfg.RedisAddr, cfg.RedisPassword)
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

	metrics.RegisterRedisCollector(redisClient, logger)

	repo := repository.NewTaskRepository(redisClient, loc, cfg.BackoffPolicy, cfg.BackoffBaseSeconds, cfg.BackoffMaxSeconds)
	subRepo := repository.NewSubscriptionRepository(redisClient, loc)
	subs := services.NewSubscriptionService(subRepo)
	notifier := services.NewNotifierService(subRepo, logger, cfg.WebhookHmacSecret, cfg.SubscriptionMinIntervalSeconds, limiter, ratelimit.Bucket(cfg.RateLimit.Webhook))
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
		limiter,
		ratelimit.Bucket(cfg.RateLimit.Webhook),
	)
	results := services.NewResultsService(resultRepo, uploader, resultCallback, logger, time.Now, loc)

	engine := gin.New()
	engine.Use(gin.Recovery(), middleware.RequestIDMiddleware(), middleware.LoggerMiddleware(logger))

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

	return app, nil
}
