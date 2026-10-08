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

const maxBatchCreateSize = 100

type batchCreateTaskController struct{ svc services.SchedulerService }

func NewBatchCreateTaskController(svc services.SchedulerService) *batchCreateTaskController {
	return &batchCreateTaskController{svc}
}

type batchCreateReq struct {
	Tasks []createReq `json:"tasks" binding:"required,min=1"`
}

type batchCreateResult struct {
	Task  *domain.Task `json:"task,omitempty"`
	Error string       `json:"error,omitempty"`
}

func (h *batchCreateTaskController) Handle(c *gin.Context) {
	var req batchCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if len(req.Tasks) > maxBatchCreateSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "batch size exceeds maximum of 100"})
		return
	}
	if denyBindingBatch(c, req.Tasks) {
		return
	}
	scope, _ := middleware.GetBindingScope(c)

	tenantID := ""
	if v, ok := c.Get("tenantID"); ok {
		if tid, ok := v.(string); ok {
			tenantID = tid
		}
	}

	results := make([]batchCreateResult, len(req.Tasks))
	for i, t := range req.Tasks {
		payloadJSON, err := jsonMarshal(t.Payload)
		if err != nil {
			results[i] = batchCreateResult{Error: err.Error()}
			continue
		}

		var runAt time.Time
		if t.RunAt != "" {
			parsed, err := time.Parse(time.RFC3339, t.RunAt)
			if err != nil {
				results[i] = batchCreateResult{Error: "invalid 'runAt' (use RFC3339)"}
				continue
			}
			runAt = parsed
		}
		if t.DelaySecs < 0 {
			results[i] = batchCreateResult{Error: "invalid 'delaySeconds' (must be >= 0)"}
			continue
		}
		if !validIdempotencyKey(t.Idempotency) {
			results[i] = batchCreateResult{Error: errInvalidIdempotency}
			continue
		}

		idempotencyKey := storageIdempotencyKey(scope, t.Idempotency)
		task, err := h.svc.CreateTask(c.Request.Context(), t.Command, payloadJSON, t.Priority, t.Webhook, t.MaxAttempts, idempotencyKey, t.TaskID, runAt, t.DelaySecs, tenantID)
		if errors.Is(err, domain.ErrIdempotencyConflict) || (err == nil && !bindingMayReplay(scope, task)) {
			// Per-item 409 equivalent: the stable code and never the task.
			recordIdempotencyConflict(c, tenantID)
			results[i] = batchCreateResult{Error: codeIdempotencyConflict}
			continue
		}
		if err != nil {
			results[i] = batchCreateResult{Error: err.Error()}
			continue
		}
		results[i] = batchCreateResult{Task: task}
	}

	c.JSON(http.StatusOK, gin.H{"results": results})
}

// denyBindingBatch authorizes every item of a Publish-binding batch before
// any enqueue. The first failing item refuses the whole batch with its index,
// so a batch is all-or-nothing for authorization (platform ADR-0022 C1.2).
func denyBindingBatch(c *gin.Context, tasks []createReq) bool {
	scope, ok := middleware.GetBindingScope(c)
	if !ok {
		return false
	}
	for i, t := range tasks {
		if code, reason := publishDenial(scope, t.Command, t.Webhook); code != "" {
			middleware.DenyBinding(c, scope, http.StatusForbidden, code, reason, gin.H{"index": i})
			return true
		}
	}
	return false
}
