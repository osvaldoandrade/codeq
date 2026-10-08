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

const (
	// adminNotFoundBody is the 404 error of GET /tasks/{id}, reused so the
	// admin task routes give no more existence information than a read.
	adminNotFoundBody = "not found"
	// repoNotFound is the repositories' missing-task sentinel message.
	repoNotFound = "not-found"
)

// ---------------- POST /admin/tasks/:id/requeue ----------------

type requeueTaskController struct{ svc services.SchedulerService }

// NewRequeueTaskController serves POST /admin/tasks/:id/requeue, which moves
// a dead-lettered task of the caller's tenant back to its ready queue
// (ADR 0009).
func NewRequeueTaskController(svc services.SchedulerService) *requeueTaskController {
	return &requeueTaskController{svc}
}

// Handle answers 200 with the requeued task, 404 when the task does not
// exist or belongs to another tenant, 409 task_not_in_dlq when it is not
// dead-lettered, or 500.
func (h *requeueTaskController) Handle(c *gin.Context) {
	taskID := c.Param("id")
	if !adminTaskVisible(c, h.svc, taskID) {
		return
	}
	task, err := h.svc.RequeueDLQTask(c.Request.Context(), taskID)
	if err != nil {
		respondAdminTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

// ---------------- DELETE /admin/tasks/:id ----------------

type deleteTaskController struct{ svc services.SchedulerService }

// NewDeleteTaskController serves DELETE /admin/tasks/:id, which removes a
// task of the caller's tenant that no worker holds (ADR 0009).
func NewDeleteTaskController(svc services.SchedulerService) *deleteTaskController {
	return &deleteTaskController{svc}
}

// Handle answers 204 once the task is gone, 404 when it does not exist or
// belongs to another tenant, 409 task_in_progress while a worker holds it,
// or 500.
func (h *deleteTaskController) Handle(c *gin.Context) {
	taskID := c.Param("id")
	if !adminTaskVisible(c, h.svc, taskID) {
		return
	}
	if err := h.svc.DeleteTask(c.Request.Context(), taskID); err != nil {
		respondAdminTaskError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// adminTaskVisible reads the task and reports whether the caller may act on
// it. It answers 404 itself for a missing task or one outside the caller's
// tenant (the GET /tasks/{id} rule), and 500 for a storage failure.
func adminTaskVisible(c *gin.Context, svc services.SchedulerService, taskID string) bool {
	task, err := svc.GetTask(c.Request.Context(), taskID)
	switch {
	case err != nil && err.Error() != repoNotFound:
		c.JSON(http.StatusInternalServerError, gin.H{errorField: err.Error()})
		return false
	case err != nil || !taskVisible(c, task):
		c.JSON(http.StatusNotFound, gin.H{errorField: adminNotFoundBody})
		return false
	}
	return true
}

// respondAdminTaskError forwards a Raft not-leader error to the leader and
// maps the other errors of the admin task routes to their statuses.
func respondAdminTaskError(c *gin.Context, err error) {
	if maybeForwardLeader(c, err) {
		return
	}
	switch {
	case errors.Is(err, domain.ErrTaskNotInDLQ), errors.Is(err, domain.ErrTaskInProgress):
		c.JSON(http.StatusConflict, gin.H{errorField: err.Error()})
	case err.Error() == repoNotFound:
		c.JSON(http.StatusNotFound, gin.H{errorField: adminNotFoundBody})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{errorField: err.Error()})
	}
}

// ---------------- POST /admin/queues/:command/dlq/requeue ----------------

type requeueDLQController struct{ svc services.SchedulerService }

// NewRequeueDLQController serves POST /admin/queues/:command/dlq/requeue,
// which requeues up to ?limit= tasks of the caller tenant's dead-letter
// queue for the command (ADR 0009).
func NewRequeueDLQController(svc services.SchedulerService) *requeueDLQController {
	return &requeueDLQController{svc}
}

// Handle answers 200 with {"requeued": n, "remaining": bool}, 400 for a
// missing command or an invalid limit, or 500. A client drains the queue
// by repeating the call while remaining is true.
func (h *requeueDLQController) Handle(c *gin.Context) {
	cmd := strings.TrimSpace(c.Param("command"))
	if cmd == "" {
		c.JSON(http.StatusBadRequest, gin.H{errorField: "command is required"})
		return
	}
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		var err error
		if limit, err = strconv.Atoi(raw); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{errorField: domain.ErrInvalidRequeueLimit.Error()})
			return
		}
	}
	out, err := h.svc.RequeueDLQ(c.Request.Context(), domain.Command(cmd), middleware.GetTenantID(c), limit)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, out)
	case maybeForwardLeader(c, err):
	case errors.Is(err, domain.ErrInvalidRequeueLimit):
		c.JSON(http.StatusBadRequest, gin.H{errorField: err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{errorField: err.Error()})
	}
}
