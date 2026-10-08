package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

// Stable error codes for binding-scoped tokens (platform ADR-0022 C1).
const (
	CodeBindingScopeDenied = "binding_scope_denied"
	CodeRouteNotAllowed    = "route_not_allowed"
	CodeEventTypeNotAllow  = "event_type_not_allowed"
	CodeWebhookNotAllowed  = "webhook_not_allowed"
	CodeNotOwner           = "not-owner"

	bindingScopeContextKey = "bindingScope"
	maxLoggedRequestID     = 128
	errorBodyKey           = "error"

	routeTasks      = "/v1/codeq/tasks"
	routeTaskByID   = routeTasks + "/:id"
	routeTasksClaim = routeTasks + "/claim"
)

type bindingRoute struct{ method, path string }

var (
	publishOnly   = []authclaims.BindingPolicy{authclaims.PolicyPublish}
	subscribeOnly = []authclaims.BindingPolicy{authclaims.PolicySubscribe}
	bothPolicies  = []authclaims.BindingPolicy{authclaims.PolicyPublish, authclaims.PolicySubscribe}

	// bindingRoutes is the complete ADR-0022 C1.2 allow-list, keyed on the
	// registered route template. Anything absent is refused, including every
	// /admin route and the subscription routes.
	bindingRoutes = map[bindingRoute][]authclaims.BindingPolicy{
		{http.MethodPost, routeTasks}:                    publishOnly,
		{http.MethodPost, routeTasks + "/batch"}:         publishOnly,
		{http.MethodGet, routeTaskByID}:                  bothPolicies,
		{http.MethodGet, routeTaskByID + "/result"}:      bothPolicies,
		{http.MethodPost, routeTasksClaim}:               subscribeOnly,
		{http.MethodPost, routeTasksClaim + "/batch"}:    subscribeOnly,
		{http.MethodPost, routeTaskByID + "/heartbeat"}:  subscribeOnly,
		{http.MethodPost, routeTaskByID + "/progress"}:   subscribeOnly,
		{http.MethodPost, routeTaskByID + "/abandon"}:    subscribeOnly,
		{http.MethodPost, routeTaskByID + "/nack"}:       subscribeOnly,
		{http.MethodPost, routeTaskByID + "/result"}:     subscribeOnly,
		{http.MethodPost, routeTasks + "/batch/results"}: subscribeOnly,
		{http.MethodGet, "/v1/codeq/raft/status"}:        bothPolicies,
	}

	// bindingClock is the verifier clock; tests replace it.
	bindingClock = time.Now
)

// GetBindingScope returns the verified binding scope of the request token.
// It is absent for every other token kind.
func GetBindingScope(c *gin.Context) (*authclaims.BindingScope, bool) {
	v, ok := c.Get(bindingScopeContextKey)
	if !ok {
		return nil, false
	}
	scope, ok := v.(*authclaims.BindingScope)
	return scope, ok && scope != nil
}

// authorizeBindingScope enforces the binding contract on an authenticated
// token. Tokens that are not binding candidates pass unchanged. It returns
// false after aborting the request.
func authorizeBindingScope(c *gin.Context, claims *auth.Claims) bool {
	if !authclaims.RequiresBindingScope(claims) {
		return true
	}
	scope, err := authclaims.ResolveBindingScope(claims, bindingClock())
	if err != nil {
		reason := authclaims.ReasonMalformedBinding
		var denied *authclaims.BindingDenied
		if errors.As(err, &denied) {
			reason = denied.Reason
		}
		DenyBinding(c, nil, http.StatusForbidden, CodeBindingScopeDenied, reason, nil)
		return false
	}
	if !bindingRouteAllowed(c.Request.Method, c.FullPath(), scope.Policy) {
		DenyBinding(c, scope, http.StatusForbidden, CodeRouteNotAllowed, CodeRouteNotAllowed, nil)
		return false
	}
	c.Set(bindingScopeContextKey, scope)
	return true
}

func bindingRouteAllowed(method, path string, policy authclaims.BindingPolicy) bool {
	for _, allowed := range bindingRoutes[bindingRoute{method: method, path: path}] {
		if allowed == policy {
			return true
		}
	}
	return false
}

// DenyBinding aborts a binding-scoped request with a stable code, records a
// bounded metric and writes one secret-free audit line. extra adds fields to
// the JSON body (for example the failing batch index).
func DenyBinding(c *gin.Context, scope *authclaims.BindingScope, status int, code, reason string, extra gin.H) {
	route := routeLabel(c)
	RecordBindingDenial(reason, route)
	attrs := []any{"event", "codeq_binding_scope_denied", "code", code, "reason", reason, "route", route, "method", c.Request.Method}
	if id := c.GetString("request_id"); id != "" {
		if len(id) > maxLoggedRequestID {
			id = id[:maxLoggedRequestID]
		}
		attrs = append(attrs, "requestId", id)
	}
	if scope != nil {
		attrs = append(attrs, "tenantId", scope.TenantID, "topicId", scope.TopicID, "policy", string(scope.Policy), "bindingUid", scope.UID)
	}
	slog.Default().Warn("binding-scoped token refused", attrs...)
	body := gin.H{errorBodyKey: code}
	for key, value := range extra {
		body[key] = value
	}
	c.AbortWithStatusJSON(status, body)
}

// routeLabel returns the registered route template, a bounded label value.
func routeLabel(c *gin.Context) string {
	if route := c.FullPath(); route != "" {
		return route
	}
	return "unmatched"
}

// RecordBindingDenial increments the bounded denial counter. It is shared
// with the gRPC stream servers.
func RecordBindingDenial(reason, route string) {
	metrics.BindingScopeDeniedTotal.WithLabelValues(reason, route).Inc()
}
