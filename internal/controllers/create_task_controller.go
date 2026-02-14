package controllers

import (
	"net/http"
	"time"

	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/domain"

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
	RunAt       string         `json:"runAt,omitempty"`        // RFC3339: when the task becomes visible to workers
	DelaySecs   int            `json:"delaySeconds,omitempty"` // convenience alternative to runAt
}

func (h *createTaskController) Handle(c *gin.Context) {
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	payloadJSON, _ := jsonMarshal(req.Payload)

	var runAt time.Time
	if req.RunAt != "" {
		t, err := time.Parse(time.RFC3339, req.RunAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid 'runAt' (use RFC3339)"})
			return
		}
		runAt = t
	}
	if req.DelaySecs < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid 'delaySeconds' (must be >= 0)"})
		return
	}

	task, err := h.svc.CreateTask(c.Request.Context(), req.Command, payloadJSON, req.Priority, req.Webhook, req.MaxAttempts, req.Idempotency, runAt, req.DelaySecs)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, task)
}
