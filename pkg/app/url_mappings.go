package app

import (
	"github.com/osvaldoandrade/codeq/internal/controllers"
	"github.com/osvaldoandrade/codeq/internal/middleware"
)

func SetupMappings(app *Application) {
	v1 := app.Engine.Group("/v1/codeq")
	producer := v1.Group("", middleware.AuthMiddleware(app.ProducerValidator, app.Config))
	worker := v1.Group("", middleware.WorkerAuthMiddleware(app.WorkerValidator, app.ProducerValidator, app.Config))
	anyAuth := v1.Group("", middleware.AnyAuthMiddleware(app.WorkerValidator, app.ProducerValidator, app.Config))
	{
		producer.POST("/tasks", controllers.NewCreateTaskController(app.Scheduler).Handle)

		worker.POST("/tasks/claim", middleware.RequireWorkerScope("codeq:claim"), controllers.NewClaimTaskController(app.Scheduler).Handle)
		worker.POST("/tasks/:id/heartbeat", middleware.RequireWorkerScope("codeq:heartbeat"), controllers.NewHeartbeatController(app.Scheduler).Handle)
		worker.POST("/tasks/:id/abandon", middleware.RequireWorkerScope("codeq:abandon"), controllers.NewAbandonController(app.Scheduler).Handle)
		worker.POST("/tasks/:id/nack", middleware.RequireWorkerScope("codeq:nack"), controllers.NewNackController(app.Scheduler).Handle)
		worker.POST("/tasks/:id/result", middleware.RequireWorkerScope("codeq:result"), controllers.NewSubmitResultController(app.Results).Handle)
		worker.POST("/workers/subscriptions", middleware.RequireWorkerScope("codeq:subscribe"), controllers.NewCreateSubscriptionController(app.Subs).Handle)
		worker.POST("/workers/subscriptions/:id/heartbeat", middleware.RequireWorkerScope("codeq:subscribe"), controllers.NewHeartbeatSubscriptionController(app.Subs).Handle)

		anyAuth.GET("/tasks/:id", controllers.NewGetTaskController(app.Scheduler).Handle)
		anyAuth.GET("/tasks/:id/result", controllers.NewGetResultController(app.Results).Handle)

		admin := producer.Group("/admin", middleware.RequireAdmin())
		admin.GET("/queues", controllers.NewQueuesAdminController(app.Scheduler).Handle)
		admin.GET("/queues/:command", controllers.NewQueueStatsController(app.Scheduler).Handle)

		// Novo: limpeza administrativa de tasks expiradas no índice Z
		admin.POST("/tasks/cleanup", controllers.NewCleanupExpiredController(app.Scheduler).Handle)
	}
}
