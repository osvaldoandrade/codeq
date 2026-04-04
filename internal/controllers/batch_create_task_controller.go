package controllers

import (
	"net/http"
	"time"

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

		task, err := h.svc.CreateTask(c.Request.Context(), t.Command, payloadJSON, t.Priority, t.Webhook, t.MaxAttempts, t.Idempotency, runAt, t.DelaySecs, tenantID)
		if err != nil {
			results[i] = batchCreateResult{Error: err.Error()}
			continue
		}
		results[i] = batchCreateResult{Task: task}
	}

	c.JSON(http.StatusOK, gin.H{"results": results})
}
