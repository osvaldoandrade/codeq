package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type abandonController struct{ svc services.SchedulerService }

func NewAbandonController(svc services.SchedulerService) *abandonController {
	return &abandonController{svc}
}

func (h *abandonController) Handle(c *gin.Context) {
	taskID := c.Param("id")
	claims, ok := middleware.GetWorkerClaims(c)
	if !ok || claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing worker claims"})
		return
	}
	if err := h.svc.Abandon(c.Request.Context(), taskID, claims.Subject); err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "not-owner" {
			status = http.StatusForbidden
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "requeued"})
}
