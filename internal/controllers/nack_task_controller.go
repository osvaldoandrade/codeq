package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type nackController struct{ svc services.SchedulerService }

func NewNackController(svc services.SchedulerService) *nackController {
	return &nackController{svc}
}

type nackReq struct {
	DelaySeconds int    `json:"delaySeconds,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func (h *nackController) Handle(c *gin.Context) {
	taskID := c.Param("id")
	var req nackReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	claims, ok := middleware.GetWorkerClaims(c)
	if !ok || claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing worker claims"})
		return
	}
	delay, movedToDLQ, err := h.svc.NackTask(c.Request.Context(), taskID, claims.Subject, req.DelaySeconds, req.Reason)
	if err != nil {
		status := http.StatusInternalServerError
		switch err.Error() {
		case "not-owner":
			status = http.StatusForbidden
		case "not-found":
			status = http.StatusNotFound
		case "not-in-progress":
			status = http.StatusConflict
		default:
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	respStatus := "requeued"
	if movedToDLQ {
		respStatus = "dlq"
	}
	c.JSON(http.StatusOK, gin.H{"status": respStatus, "delaySeconds": delay})
}
