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
	return &createTaskController{svc: svc}
}

type createReq struct {
	Command     domain.Command `json:"command" binding:"required"`
	Payload     any            `json:"payload" binding:"required"`
	Priority    int            `json:"priority"`
	Webhook     string         `json:"webhook,omitempty"`
	MaxAttempts int            `json:"maxAttempts,omitempty"`
	Idempotency string         `json:"idempotencyKey,omitempty"`
	RunAt       string         `json:"runAt,omitempty"`
	DelaySecs   int            `json:"delaySeconds,omitempty"`
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

	// Extract tenant ID from the request context
	tenantID := ""
	if v, ok := c.Get("tenantID"); ok {
		if tid, ok := v.(string); ok {
			tenantID = tid
		}
	}

	task, err := h.svc.CreateTask(c.Request.Context(), req.Command, payloadJSON, req.Priority, req.Webhook, req.MaxAttempts, req.Idempotency, runAt, req.DelaySecs, tenantID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, task)
}
