package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/gin-gonic/gin"
)

const maxBatchClaimSize = 10

type batchClaimTaskController struct{ svc services.SchedulerService }

func NewBatchClaimTaskController(svc services.SchedulerService) *batchClaimTaskController {
	return &batchClaimTaskController{svc}
}

type batchClaimReq struct {
	Commands     []domain.Command `json:"commands,omitempty"`
	LeaseSeconds int              `json:"leaseSeconds,omitempty"`
	Count        int              `json:"count" binding:"required,min=1"`
}

func (h *batchClaimTaskController) Handle(c *gin.Context) {
	var req batchClaimReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if req.Count > maxBatchClaimSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "count exceeds maximum of 10"})
		return
	}

	claims, okClaims := middleware.GetWorkerClaims(c)
	if !okClaims || claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing worker claims"})
		return
	}

	allowed := map[domain.Command]bool{}
	hasWildcard := false
	for _, ev := range claims.EventTypes {
		if ev == "*" {
			hasWildcard = true
			break
		}
		allowed[domain.Command(ev)] = true
	}
	if len(req.Commands) > 0 {
		if !hasWildcard {
			for _, cmd := range req.Commands {
				if !allowed[cmd] {
					c.JSON(http.StatusForbidden, gin.H{"error": "event type not allowed"})
					return
				}
			}
		}
	} else {
		if !hasWildcard {
			for cmd := range allowed {
				req.Commands = append(req.Commands, cmd)
			}
		}
	}

	tenantID := ""
	if v, ok := c.Get("tenantID"); ok {
		if tid, ok := v.(string); ok {
			tenantID = tid
		}
	}

	var tasks []*domain.Task
	var claimErr string
	for i := 0; i < req.Count; i++ {
		task, ok, err := h.svc.ClaimTask(c.Request.Context(), claims.Subject, req.Commands, req.LeaseSeconds, 0, tenantID)
		if err != nil {
			claimErr = err.Error()
			break
		}
		if !ok {
			break
		}
		tasks = append(tasks, task)
	}

	if len(tasks) == 0 {
		if claimErr != "" {
			c.JSON(http.StatusInternalServerError, gin.H{"error": claimErr})
			return
		}
		c.Status(http.StatusNoContent)
		return
	}
	resp := gin.H{"tasks": tasks}
	if claimErr != "" {
		resp["error"] = claimErr
	}
	c.JSON(http.StatusOK, resp)
}
