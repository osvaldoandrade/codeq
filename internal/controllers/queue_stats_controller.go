package controllers

import (
	"net/http"
	"strings"

	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/gin-gonic/gin"
)

type queueStatsController struct{ svc services.SchedulerService }

func NewQueueStatsController(svc services.SchedulerService) *queueStatsController {
	return &queueStatsController{svc: svc}
}

func (h *queueStatsController) Handle(c *gin.Context) {
	cmd := strings.TrimSpace(c.Param("command"))
	if cmd == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "command is required"})
		return
	}
	out, err := h.svc.QueueStats(c.Request.Context(), domain.Command(cmd))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}
