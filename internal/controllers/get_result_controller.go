package controllers

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

type getResultController struct{ svc services.ResultsService }

func NewGetResultController(s services.ResultsService) *getResultController {
	return &getResultController{svc: s}
}

func (h *getResultController) Handle(c *gin.Context) {
	id := c.Param("id")
	res, task, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": task, "result": res})
}
