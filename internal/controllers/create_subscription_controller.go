package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/gin-gonic/gin"
)

type createSubscriptionController struct{ svc services.SubscriptionService }

func NewCreateSubscriptionController(svc services.SubscriptionService) *createSubscriptionController {
	return &createSubscriptionController{svc: svc}
}

type createSubscriptionReq struct {
	CallbackURL        string           `json:"callbackUrl" binding:"required"`
	EventTypes         []domain.Command `json:"eventTypes,omitempty"`
	TTLSeconds         int              `json:"ttlSeconds,omitempty"`
	DeliveryMode       string           `json:"deliveryMode,omitempty"`
	GroupID            string           `json:"groupId,omitempty"`
	MinIntervalSeconds int              `json:"minIntervalSeconds,omitempty"`
}

func (h *createSubscriptionController) Handle(c *gin.Context) {
	var req createSubscriptionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	sub, err := h.svc.Create(c.Request.Context(), req.CallbackURL, req.EventTypes, req.DeliveryMode, req.GroupID, req.TTLSeconds, req.MinIntervalSeconds)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"subscriptionId": sub.ID,
		"expiresAt":      sub.ExpiresAt,
	})
}
