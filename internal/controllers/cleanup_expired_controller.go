package controllers

import (
	"net/http"
	"time"

	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type cleanupExpiredController struct{ svc services.SchedulerService }

func NewCleanupExpiredController(svc services.SchedulerService) *cleanupExpiredController {
	return &cleanupExpiredController{svc}
}

type cleanupReq struct {
	Limit  int    `json:"limit,omitempty"`  // default: 1000
	Before string `json:"before,omitempty"` // RFC3339; default: now
}

func (h *cleanupExpiredController) Handle(c *gin.Context) {
	var req cleanupReq
	_ = c.ShouldBindJSON(&req) // parâmetros são opcionais

	before := time.Now().UTC()
	if req.Before != "" {
		if t, err := time.Parse(time.RFC3339, req.Before); err == nil {
			before = t
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid 'before' (use RFC3339)"})
			return
		}
	}

	deleted, err := h.svc.CleanupExpired(c.Request.Context(), req.Limit, before)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"deleted": deleted,
		"before":  before.Format(time.RFC3339),
		"limit":   req.Limit,
	})
}
