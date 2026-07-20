// Package topics exposes QueueTopic administration over HTTP.
package topics

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	errorKey        = "error"
	maxRequestBytes = 1 << 20
)

// Service is the transport-owned use-case boundary.
type Service interface {
	Upsert(context.Context, string, string, queuetopic.Policy) (queuetopic.Topic, bool, bool, error)
	Get(context.Context, string, string) (queuetopic.Topic, error)
	Delete(context.Context, string, string) error
}

// Handler translates the topic use cases to HTTP.
type Handler struct {
	service Service
}

// NewHandler builds the topic administration handler.
func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// Upsert handles an idempotent topic policy PUT.
func (h *Handler) Upsert(c *gin.Context) {
	var policy queuetopic.Policy
	if err := decodePolicy(c, &policy); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{errorKey: "invalid request body: " + err.Error()})
		return
	}

	topic, created, _, err := h.service.Upsert(c.Request.Context(), middleware.GetTenantID(c), c.Param("topicName"), policy)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, topic)
}

// Get handles a tenant-scoped topic lookup.
func (h *Handler) Get(c *gin.Context) {
	topic, err := h.service.Get(c.Request.Context(), middleware.GetTenantID(c), c.Param("topicName"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, topic)
}

// Delete requires the caller to state the destructive provider policy.
func (h *Handler) Delete(c *gin.Context) {
	if c.Query("deletionPolicy") != "Delete" {
		c.JSON(http.StatusBadRequest, gin.H{errorKey: "deletionPolicy=Delete is required"})
		return
	}
	if err := h.service.Delete(c.Request.Context(), middleware.GetTenantID(c), c.Param("topicName")); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func decodePolicy(c *gin.Context, policy *queuetopic.Policy) error {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(policy); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("only one JSON object is allowed")
		}
		return err
	}
	return nil
}

func writeError(c *gin.Context, err error) {
	var leaderHint domain.LeaderHint
	if errors.As(err, &leaderHint) {
		leader := leaderHint.LeaderHTTPAddr()
		if leader == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{errorKey: "queue topic leader unavailable"})
			return
		}
		location := strings.TrimSuffix(leader, "/") + c.Request.URL.RequestURI()
		c.Header("Location", location)
		c.JSON(http.StatusTemporaryRedirect, gin.H{errorKey: "not leader", "leader": leader})
		return
	}
	var validation *queuetopic.ValidationError
	var notFound *queuetopic.NotFoundError
	var conflict *queuetopic.ConflictError
	var unavailable *queuetopic.UnavailableError
	switch {
	case errors.As(err, &validation):
		c.JSON(http.StatusUnprocessableEntity, gin.H{errorKey: validation.Error(), "field": validation.Field})
	case errors.As(err, &notFound):
		c.JSON(http.StatusNotFound, gin.H{errorKey: notFound.Error()})
	case errors.As(err, &conflict):
		c.JSON(http.StatusConflict, gin.H{errorKey: conflict.Error()})
	case errors.As(err, &unavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{errorKey: unavailable.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{errorKey: "queue topic operation failed"})
	}
}
