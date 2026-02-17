package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type heartbeatController struct{ svc services.SchedulerService }

func NewHeartbeatController(svc services.SchedulerService) *heartbeatController {
	return &heartbeatController{svc: svc}
}

type hbReq struct {
	ExtendSeconds int `json:"extendSeconds,omitempty"`
}

func (h *heartbeatController) Handle(c *gin.Context) {
	taskID := c.Param("id")
	var req hbReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	claims, ok := middleware.GetWorkerClaims(c)
	if !ok || claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing worker claims"})
		return
	}
	if err := h.svc.Heartbeat(c.Request.Context(), taskID, claims.Subject, req.ExtendSeconds); err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "not-owner" {
			status = http.StatusForbidden
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
