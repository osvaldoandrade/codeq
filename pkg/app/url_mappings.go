package app

import (
	topicsapp "github.com/osvaldoandrade/codeq/internal/application/topics"
	"github.com/osvaldoandrade/codeq/internal/controllers"
	"github.com/osvaldoandrade/codeq/internal/middleware"
	topicshttp "github.com/osvaldoandrade/codeq/internal/server/http/topics"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func SetupMappings(app *Application) {
	app.Engine.GET("/metrics", gin.WrapH(promhttp.Handler()))

	v1 := app.Engine.Group("/v1/codeq")
	producer := v1.Group("", middleware.AuthMiddleware(app.ProducerValidator, app.Config))
	worker := v1.Group("", middleware.WorkerAuthMiddleware(app.WorkerValidator, app.ProducerValidator, app.Config))
	anyAuth := v1.Group("", middleware.AnyAuthMiddleware(app.WorkerValidator, app.ProducerValidator, app.Config))
	{
		producer.POST("/tasks", middleware.RateLimitProducer(app.RateLimiter, app.Config), controllers.NewCreateTaskController(app.Scheduler).Handle)
		producer.POST("/tasks/batch", middleware.RateLimitProducer(app.RateLimiter, app.Config), controllers.NewBatchCreateTaskController(app.Scheduler).Handle)

		worker.POST("/tasks/claim", middleware.RequireWorkerScope("codeq:claim"), middleware.RateLimitWorkerClaim(app.RateLimiter, app.Config), controllers.NewClaimTaskController(app.Scheduler).Handle)
		worker.POST("/tasks/claim/batch", middleware.RequireWorkerScope("codeq:claim"), middleware.RateLimitWorkerClaim(app.RateLimiter, app.Config), controllers.NewBatchClaimTaskController(app.Scheduler).Handle)
		worker.POST("/tasks/:id/heartbeat", middleware.RequireWorkerScope("codeq:heartbeat"), controllers.NewHeartbeatController(app.Scheduler).Handle)
		worker.POST("/tasks/:id/abandon", middleware.RequireWorkerScope("codeq:abandon"), controllers.NewAbandonController(app.Scheduler).Handle)
		worker.POST("/tasks/:id/nack", middleware.RequireWorkerScope("codeq:nack"), controllers.NewNackController(app.Scheduler).Handle)
		worker.POST("/tasks/:id/result", middleware.RequireWorkerScope("codeq:result"), controllers.NewSubmitResultController(app.Results).Handle)
		worker.POST("/tasks/batch/results", middleware.RequireWorkerScope("codeq:result"), controllers.NewBatchSubmitResultController(app.Results).Handle)
		worker.POST("/workers/subscriptions", middleware.RequireWorkerScope("codeq:subscribe"), controllers.NewCreateSubscriptionController(app.Subs).Handle)
		worker.POST("/workers/subscriptions/:id/heartbeat", middleware.RequireWorkerScope("codeq:subscribe"), controllers.NewHeartbeatSubscriptionController(app.Subs).Handle)

		anyAuth.GET("/tasks/:id", controllers.NewGetTaskController(app.Scheduler).Handle)
		anyAuth.GET("/tasks/:id/result", controllers.NewGetResultController(app.Results).Handle)

		// Raft status — local-node view of per-shard leadership.
		// Public (anyAuth) so ops tooling and Prometheus scrapers
		// can poll without an admin token. The payload reveals no
		// task data, only routing metadata (peer IDs + bind addrs).
		anyAuth.GET("/raft/status", controllers.NewRaftStatusController(adaptRaftGroups(app.RaftGroups)).Handle)

		admin := producer.Group("/admin", middleware.RequireAdmin())
		topicService := app.Topics
		if topicService == nil {
			topicService = topicsapp.NewUnavailableService("topic service not configured")
		}
		topicHandler := topicshttp.NewHandler(topicService)
		admin.PUT("/topics/:topicName", topicHandler.Upsert)
		admin.GET("/topics/:topicName", topicHandler.Get)
		admin.DELETE("/topics/:topicName", topicHandler.Delete)
		admin.GET("/queues", controllers.NewQueuesAdminController(app.Scheduler).Handle)
		admin.GET("/queues/:command", controllers.NewQueueStatsController(app.Scheduler).Handle)

		// Novo: limpeza administrativa de tasks expiradas no índice Z
		admin.POST("/tasks/cleanup", middleware.RateLimitAdminCleanup(app.RateLimiter, app.Config), controllers.NewCleanupExpiredController(app.Scheduler).Handle)
	}
}

// adaptRaftGroups converts the public app.RaftGroupStatus slice to the
// controllers package's mirror interface. Both have the same method
// set so the conversion is a no-op wrapper.
func adaptRaftGroups(in []RaftGroupStatus) []controllers.RaftGroupStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]controllers.RaftGroupStatus, len(in))
	for i, g := range in {
		out[i] = g
	}
	return out
}
