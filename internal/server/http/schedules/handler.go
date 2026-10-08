// Package schedules exposes recurring schedule administration over HTTP.
package schedules

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
	"github.com/osvaldoandrade/codeq/internal/leaderforward"
	"github.com/osvaldoandrade/codeq/internal/middleware"
)

const (
	errorKey        = "error"
	maxRequestBytes = 1 << 20
	nameParam       = "name"
)

// Service is the transport-owned use-case boundary.
type Service interface {
	Upsert(context.Context, string, string, schedule.Spec) (schedule.Schedule, bool, error)
	Get(context.Context, string, string) (schedule.Schedule, error)
	List(context.Context, string) ([]schedule.Schedule, error)
	Delete(context.Context, string, string) error
}

// Handler translates the schedule use cases to HTTP.
type Handler struct {
	service Service
}

// NewHandler builds the schedule administration handler.
func NewHandler(service Service) *Handler {
	return &Handler{service: service}
}

// Upsert handles an idempotent PUT of a schedule spec: 201 on create, 200
// on an identical replay or an update.
func (h *Handler) Upsert(c *gin.Context) {
	var spec schedule.Spec
	if err := decodeSpec(c, &spec); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{errorKey: "invalid request body: " + err.Error()})
		return
	}
	sc, created, err := h.service.Upsert(c.Request.Context(), middleware.GetTenantID(c), c.Param(nameParam), spec)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, sc)
}

// Get handles a tenant-scoped schedule lookup.
func (h *Handler) Get(c *gin.Context) {
	sc, err := h.service.Get(c.Request.Context(), middleware.GetTenantID(c), c.Param(nameParam))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sc)
}

// List handles GET of every schedule of the tenant.
func (h *Handler) List(c *gin.Context) {
	all, err := h.service.List(c.Request.Context(), middleware.GetTenantID(c))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedules": all})
}

// Delete removes a schedule; deleting an absent one also answers 204.
func (h *Handler) Delete(c *gin.Context) {
	if err := h.service.Delete(c.Request.Context(), middleware.GetTenantID(c), c.Param(nameParam)); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func decodeSpec(c *gin.Context, spec *schedule.Spec) error {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(spec); err != nil {
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
	// A follower forwards the write to the configured leader in-process or
	// answers 503 leader_unavailable; it never answers 307.
	if leaderforward.HandleNotLeader(c, err) {
		return
	}
	var validation *schedule.ValidationError
	var notFound *schedule.NotFoundError
	var unavailable *schedule.UnavailableError
	switch {
	case errors.As(err, &validation):
		c.JSON(http.StatusUnprocessableEntity, gin.H{errorKey: validation.Error(), "field": validation.Field})
	case errors.As(err, &notFound):
		c.JSON(http.StatusNotFound, gin.H{errorKey: notFound.Error()})
	case errors.As(err, &unavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{errorKey: unavailable.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{errorKey: "schedule operation failed"})
	}
}
