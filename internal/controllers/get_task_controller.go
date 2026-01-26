package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type getTaskController struct{ svc services.SchedulerService }

func NewGetTaskController(svc services.SchedulerService) *getTaskController {
	return &getTaskController{svc}
}

func (h *getTaskController) Handle(c *gin.Context) {
	taskID := c.Param("id")
	task, err := h.svc.GetTask(c.Request.Context(), taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, task)
}
