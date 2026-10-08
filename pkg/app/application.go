package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	schedulesapp "github.com/osvaldoandrade/codeq/internal/application/schedules"
	topicsapp "github.com/osvaldoandrade/codeq/internal/application/topics"
	"github.com/osvaldoandrade/codeq/internal/leaderforward"
	"github.com/osvaldoandrade/codeq/internal/ratelimit"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"
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
	Topics            *topicsapp.Service
	Schedules         *schedulesapp.Service
	Logger            *slog.Logger
	TZ                *time.Location
	ProducerValidator auth.Validator
	WorkerValidator   auth.Validator
	RateLimiter       ratelimit.Limiter
	// RaftGroups, when non-nil, is the per-shard raft state in raft
	// mode. Index = shardIdx. Empty when raft is disabled.
	RaftGroups []RaftGroupStatus
	// LeaderForward relays follower writes to the Raft leader in-process
	// (platform ADR-0022 C1.5). Nil when Raft is disabled; a nil
	// forwarder passes requests through.
	LeaderForward   *leaderforward.Forwarder
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

// pebblePersistenceProvider is the only backend NewApplication opens.
const pebblePersistenceProvider = "pebble"

// NewApplication builds a CodeQ process that persists on Pebble.
func NewApplication(cfg *config.Config, opts ...ApplicationOption) (*Application, error) {
	if cfg.PersistenceProvider != "" && cfg.PersistenceProvider != pebblePersistenceProvider {
		return nil, fmt.Errorf("persistence provider %q is not supported; CodeQ persists on pebble", cfg.PersistenceProvider)
	}
	return newPebbleApplication(cfg, opts...)
}
