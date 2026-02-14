package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/gin-gonic/gin"
)

type submitResultController struct{ svc services.ResultsService }

func NewSubmitResultController(s services.ResultsService) *submitResultController {
	return &submitResultController{svc: s}
}

func (h *submitResultController) Handle(c *gin.Context) {
	id := c.Param("id")
	var req domain.SubmitResultRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	claims, ok := middleware.GetWorkerClaims(c)
	if !ok || claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing worker claims"})
		return
	}
	req.WorkerID = claims.Subject
	rec, err := h.svc.Submit(c.Request.Context(), id, req)
	if err != nil {
		status := http.StatusBadRequest
		switch err.Error() {
		case "task not found":
			status = http.StatusNotFound
		case "not-owner":
			status = http.StatusForbidden
		case "not-in-progress":
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rec)
}
