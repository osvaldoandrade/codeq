package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type createTaskController struct{ svc services.SchedulerService }

func NewCreateTaskController(svc services.SchedulerService) *createTaskController {
	return &createTaskController{svc}
}

type createReq struct {
	Command     domain.Command `json:"command" binding:"required"`
	Payload     interface{}    `json:"payload" binding:"required"` // objeto arbitrário
	Priority    int            `json:"priority"`
	Webhook     string         `json:"webhook,omitempty"`
	MaxAttempts int            `json:"maxAttempts,omitempty"`
	Idempotency string         `json:"idempotencyKey,omitempty"`
}

func (h *createTaskController) Handle(c *gin.Context) {
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	payloadJSON, _ := jsonMarshal(req.Payload)
	task, err := h.svc.CreateTask(c.Request.Context(), req.Command, payloadJSON, req.Priority, req.Webhook, req.MaxAttempts, req.Idempotency)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, task)
}
