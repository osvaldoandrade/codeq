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

	// Convert to service batch items
	items := make([]domain.BatchSubmitItem, len(req.Results))
	for i, item := range req.Results {
		item.SubmitResultRequest.WorkerID = claims.Subject
		items[i] = domain.BatchSubmitItem{
			TaskID:              item.TaskID,
			SubmitResultRequest: item.SubmitResultRequest,
		}
	}

	// Use batch submit for optimized RTT reduction
	responses, err := h.svc.BatchSubmit(c.Request.Context(), items)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "batch submit failed"})
		return
	}

	// Convert to response format
	batchResults := make([]batchSubmitResult, len(responses))
	for i, resp := range responses {
		if resp.Error != "" {
			batchResults[i] = batchSubmitResult{TaskID: resp.TaskID, Error: resp.Error}
		} else {
			batchResults[i] = batchSubmitResult{TaskID: resp.TaskID, Result: resp.Result}
		}
	}

	c.JSON(http.StatusOK, gin.H{"results": batchResults})
}
