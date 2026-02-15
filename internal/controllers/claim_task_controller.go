package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/gin-gonic/gin"
)

type claimTaskController struct{ svc services.SchedulerService }

func NewClaimTaskController(svc services.SchedulerService) *claimTaskController {
	return &claimTaskController{svc}
}

type claimReq struct {
	Commands     []domain.Command `json:"commands,omitempty"`
	LeaseSeconds int              `json:"leaseSeconds,omitempty"`
	WaitSeconds  int              `json:"waitSeconds,omitempty"`
}

func (h *claimTaskController) Handle(c *gin.Context) {
	var req claimReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
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
	// Extract tenant ID from the request context
	tenantID := ""
	if v, ok := c.Get("tenantID"); ok {
		if tid, ok := v.(string); ok {
			tenantID = tid
		}
	}
	
	task, ok, err := h.svc.ClaimTask(c.Request.Context(), claims.Subject, req.Commands, req.LeaseSeconds, req.WaitSeconds, tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.Status(http.StatusNoContent)
		return
	}
	c.JSON(http.StatusOK, task)
}
