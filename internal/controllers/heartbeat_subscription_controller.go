package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type heartbeatSubscriptionController struct{ svc services.SubscriptionService }

func NewHeartbeatSubscriptionController(svc services.SubscriptionService) *heartbeatSubscriptionController {
	return &heartbeatSubscriptionController{svc: svc}
}

type heartbeatSubReq struct {
	TTLSeconds int `json:"ttlSeconds,omitempty"`
}

func (h *heartbeatSubscriptionController) Handle(c *gin.Context) {
	id := c.Param("id")
	var req heartbeatSubReq
	_ = c.ShouldBindJSON(&req)

	sub, err := h.svc.Heartbeat(c.Request.Context(), id, req.TTLSeconds)
	if err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "not-found" {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"subscriptionId": sub.ID,
		"expiresAt":      sub.ExpiresAt,
	})
}
