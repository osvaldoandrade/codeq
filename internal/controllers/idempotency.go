package controllers

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/middleware"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	// idempotencyKeySep separates the components of a namespaced key. The
	// client key may not contain it, so the namespace cannot be forged.
	idempotencyKeySep = "\x00"

	codeIdempotencyConflict = "idempotency_conflict"
	errorField              = "error"
	errInvalidIdempotency   = "invalid 'idempotencyKey'"
	errCommandRequired      = "command is required"
	maxLoggedRequestID      = 128
	unmatchedRoute          = "unmatched"
	conflictKindBinding     = "binding"
	conflictKindToken       = "token"
)

// validIdempotencyKey rejects a client key containing NUL for every token
// kind, so no caller can write a key inside a binding namespace.
func validIdempotencyKey(key string) bool {
	return !strings.Contains(key, idempotencyKeySep)
}

// storageIdempotencyKey returns the key the repository indexes.
//
// For a Publish binding token the exact format is
//
//	tid + "\x00" + eventType + "\x00" + key
//
// where tid and eventType are the verified token's tenant and single event
// type (both restricted to [a-z0-9-], so they never contain NUL). Two topics
// of one tenant therefore never collide, and a binding key never collides
// with a legacy key. Every other token kind keeps the client key unchanged
// (the legacy, un-namespaced index): replay stays limited to the caller's
// tenant by repository.ReplayIdempotent. An empty key stays empty.
func storageIdempotencyKey(scope *authclaims.BindingScope, key string) string {
	if scope == nil || key == "" {
		return key
	}
	return scope.TenantID + idempotencyKeySep + scope.EventType + idempotencyKeySep + key
}

// bindingMayReplay is the defense-in-depth check on what a create returned
// to a Publish binding token: only a task of its own tenant and event type.
func bindingMayReplay(scope *authclaims.BindingScope, task *domain.Task) bool {
	return scope == nil || (task != nil && task.TenantID == scope.TenantID && string(task.Command) == scope.EventType)
}

// respondIdempotencyConflict answers 409 {"error":"idempotency_conflict"}
// with no task, records a bounded metric and writes one secret-free audit
// line (never the idempotency key, task ID, payload or token).
func respondIdempotencyConflict(c *gin.Context, tenantID string) {
	recordIdempotencyConflict(c, tenantID)
	c.AbortWithStatusJSON(http.StatusConflict, gin.H{errorField: codeIdempotencyConflict})
}

func recordIdempotencyConflict(c *gin.Context, tenantID string) {
	route := c.FullPath()
	if route == "" {
		route = unmatchedRoute
	}
	kind := conflictKindToken
	attrs := []any{"event", "codeq_idempotency_conflict", "route", route, "method", c.Request.Method, "tenantId", tenantID}
	if scope, ok := middleware.GetBindingScope(c); ok {
		kind = conflictKindBinding
		attrs = append(attrs, "topicId", scope.TopicID, "bindingUid", scope.UID)
	}
	if id := c.GetString("request_id"); id != "" {
		if len(id) > maxLoggedRequestID {
			id = id[:maxLoggedRequestID]
		}
		attrs = append(attrs, "requestId", id)
	}
	metrics.IdempotencyConflictTotal.WithLabelValues(route, kind).Inc()
	slog.Default().Warn("idempotency key refused", attrs...)
}
