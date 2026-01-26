package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type queuesAdminController struct{ svc services.SchedulerService }

func NewQueuesAdminController(svc services.SchedulerService) *queuesAdminController {
	return &queuesAdminController{svc}
}

func (h *queuesAdminController) Handle(c *gin.Context) {
	out, err := h.svc.AdminQueues(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}
