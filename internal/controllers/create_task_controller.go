package controllers

import (
	"errors"
	"net/http"
	"time"

	"github.com/osvaldoandrade/codeq/internal/middleware"
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
	Payload     any            `json:"payload" binding:"required"`
	Priority    int            `json:"priority"`
	Webhook     string         `json:"webhook,omitempty"`
	MaxAttempts int            `json:"maxAttempts,omitempty"`
	Idempotency string         `json:"idempotencyKey,omitempty"`
	RunAt       string         `json:"runAt,omitempty"`
	DelaySecs   int            `json:"delaySeconds,omitempty"`
	// Deduplication collapses this create into a waiting task of the same
	// tenant, command and key (ADR 0004).
	Deduplication string `json:"deduplicationKey,omitempty"`
	// TaskID is the ID the client chose (ADR 0008). Exclusive with
	// Idempotency and Deduplication.
	TaskID string `json:"taskId,omitempty"`
}

func (h *createTaskController) Handle(c *gin.Context) {
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	scope, _ := middleware.GetBindingScope(c)
	if scope != nil {
		if code, reason := publishDenial(scope, req.Command, req.Webhook); code != "" {
			middleware.DenyBinding(c, scope, http.StatusForbidden, code, reason, nil)
			return
		}
	}
	if !validIdempotencyKey(req.Idempotency) {
		c.JSON(http.StatusBadRequest, gin.H{errorField: errInvalidIdempotency})
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

	idempotencyKey := storageIdempotencyKey(scope, req.Idempotency)
	task, err := h.svc.CreateTask(c.Request.Context(), req.Command, payloadJSON, req.Priority, req.Webhook, req.MaxAttempts, idempotencyKey, req.Deduplication, req.TaskID, runAt, req.DelaySecs, tenantID)
	if errors.Is(err, domain.ErrTaskIDConflict) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": domain.ErrTaskIDConflict.Error()})
		return
	}
	if errors.Is(err, domain.ErrIdempotencyConflict) || (err == nil && !bindingMayReplay(scope, task)) {
		respondIdempotencyConflict(c, tenantID)
		return
	}
	if err != nil {
		respondWriteError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, task)
}
