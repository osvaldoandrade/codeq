package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

type listTasksController struct{ svc services.SchedulerService }

// NewListTasksController serves GET /admin/queues/:command/tasks: one page
// of the tasks in a queue state of the caller's tenant (ADR 0005).
func NewListTasksController(svc services.SchedulerService) *listTasksController {
	return &listTasksController{svc}
}

// Handle validates the state, limit and cursor query parameters and answers
// 200 with a domain.TaskPage, 400 for invalid input, or 500.
func (h *listTasksController) Handle(c *gin.Context) {
	cmd := strings.TrimSpace(c.Param("command"))
	if cmd == "" {
		c.JSON(http.StatusBadRequest, gin.H{errorField: errCommandRequired})
		return
	}
	state, err := domain.ParseQueueState(c.Query("state"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{errorField: err.Error()})
		return
	}
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{errorField: domain.ErrInvalidListLimit.Error()})
			return
		}
	}
	page, err := h.svc.ListTasks(c.Request.Context(), domain.Command(cmd), middleware.GetTenantID(c), state, limit, c.Query("cursor"))
	switch {
	case errors.Is(err, domain.ErrInvalidListLimit), errors.Is(err, domain.ErrInvalidCursor), errors.Is(err, domain.ErrInvalidQueueState):
		c.JSON(http.StatusBadRequest, gin.H{errorField: err.Error()})
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{errorField: err.Error()})
	default:
		c.JSON(http.StatusOK, page)
	}
}
