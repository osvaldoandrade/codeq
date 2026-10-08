package controllers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/internal/services"
)

const (
	// maxProgressBytes caps a progress value after compaction. Progress is
	// rewritten into the task body on every report, so it stays small.
	maxProgressBytes = 64 << 10
	// maxProgressRequestBytes caps the raw request body, leaving room for
	// whitespace that compaction removes.
	maxProgressRequestBytes = 1 << 20

	msgMissingWorkerClaims = "missing worker claims"
	codeNotFound           = "not-found"
	codeNotInProgress      = "not-in-progress"
)

var (
	errProgressMissing  = errors.New("progress is required")
	errProgressTooLarge = errors.New("progress too large")
)

type progressController struct{ svc services.SchedulerService }

// NewProgressController handles POST /v1/codeq/tasks/:id/progress, where the
// worker holding the lease reports an arbitrary JSON progress value.
func NewProgressController(svc services.SchedulerService) *progressController {
	return &progressController{svc}
}

type progressReq struct {
	Progress json.RawMessage `json:"progress"`
}

// Handle stores the progress value. A body that is not exactly one
// {"progress": <JSON>} object answers 400; a value above maxProgressBytes
// after compaction answers 413. Ownership errors map like nack: 403
// not-owner, 404 not-found and 409 not-in-progress.
func (h *progressController) Handle(c *gin.Context) {
	taskID := c.Param("id")
	progress, err := decodeProgress(c)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errProgressTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		c.JSON(status, gin.H{errorField: err.Error()})
		return
	}
	claims, ok := middleware.GetWorkerClaims(c)
	if !ok || claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{errorField: msgMissingWorkerClaims})
		return
	}
	if denyUnlessBindingOwner(c, taskID, schedulerLookup(h.svc.GetTask)) {
		return
	}
	if err := h.svc.ReportProgress(c.Request.Context(), taskID, claims.Subject, progress); err != nil {
		if maybeForwardLeader(c, err) {
			return
		}
		c.JSON(progressErrorStatus(err), gin.H{errorField: err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// decodeProgress strictly decodes a single {"progress": <JSON>} object and
// returns the compacted value. JSON null counts as missing.
func decodeProgress(c *gin.Context) (json.RawMessage, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxProgressRequestBytes))
	decoder.DisallowUnknownFields()
	var req progressReq
	if err := decoder.Decode(&req); err != nil {
		return nil, progressDecodeError(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("only one JSON object is allowed")
		}
		return nil, progressDecodeError(err)
	}
	if len(req.Progress) == 0 || string(req.Progress) == "null" {
		return nil, errProgressMissing
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, req.Progress); err != nil {
		return nil, err
	}
	if compact.Len() > maxProgressBytes {
		return nil, errProgressTooLarge
	}
	return compact.Bytes(), nil
}

// progressDecodeError reports a body cut by MaxBytesReader as too large and
// every other decode failure as an invalid body.
func progressDecodeError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return errProgressTooLarge
	}
	return errors.New("invalid body: " + err.Error())
}

// progressErrorStatus maps the repository sentinels to HTTP statuses.
func progressErrorStatus(err error) int {
	switch err.Error() {
	case middleware.CodeNotOwner:
		return http.StatusForbidden
	case codeNotFound:
		return http.StatusNotFound
	case codeNotInProgress:
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}
