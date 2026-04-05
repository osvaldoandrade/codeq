package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"github.com/gin-gonic/gin"
)

const maxBatchResultSize = 100

type batchSubmitResultController struct{ svc services.ResultsService }

func NewBatchSubmitResultController(s services.ResultsService) *batchSubmitResultController {
	return &batchSubmitResultController{svc: s}
}

type batchResultItem struct {
	TaskID string `json:"taskId" binding:"required"`
	domain.SubmitResultRequest
}

type batchSubmitReq struct {
	Results []batchResultItem `json:"results" binding:"required,min=1"`
}

type batchSubmitResult struct {
	TaskID string               `json:"taskId"`
	Result *domain.ResultRecord `json:"result,omitempty"`
	Error  string               `json:"error,omitempty"`
}

func (h *batchSubmitResultController) Handle(c *gin.Context) {
	var req batchSubmitReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if len(req.Results) > maxBatchResultSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "batch size exceeds maximum of 100"})
		return
	}

	claims, ok := middleware.GetWorkerClaims(c)
	if !ok || claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing worker claims"})
		return
	}

	results := make([]batchSubmitResult, len(req.Results))
	for i, item := range req.Results {
		item.SubmitResultRequest.WorkerID = claims.Subject
		rec, err := h.svc.Submit(c.Request.Context(), item.TaskID, item.SubmitResultRequest)
		if err != nil {
			results[i] = batchSubmitResult{TaskID: item.TaskID, Error: err.Error()}
			continue
		}
		results[i] = batchSubmitResult{TaskID: item.TaskID, Result: rec}
	}

	c.JSON(http.StatusOK, gin.H{"results": results})
}
