package middleware

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

// workerClaimScope authorizes the claim routes.
const workerClaimScope = "codeq:claim"

// promotedWorkerScopes is the full worker scope set granted to a producer
// token promoted by ALLOW_PRODUCER_AS_WORKER (unchanged legacy behavior).
var promotedWorkerScopes = []string{workerClaimScope, "codeq:heartbeat", "codeq:abandon", "codeq:nack", "codeq:result", "codeq:subscribe"}

// WorkerAuthMiddleware creates worker authentication middleware with the provided validators
func WorkerAuthMiddleware(workerValidator, producerValidator auth.Validator, cfg *config.Config) gin.HandlerFunc {
	if workerValidator == nil {
		return func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "worker validator not configured"})
		}
	}

	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		claims, err := validateBearer(workerValidator, header)
		if err == nil && claims.HasScope(topicManageScope) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		// A binding-scoped worker token is judged only by the binding
		// contract; it never falls back to producer promotion.
		if err == nil && !authclaims.RequiresBindingScope(claims) {
			err = validateWorkerShape(claims)
		}
		if err != nil {
			promoted, status, promotionErr := promoteProducerToWorker(c, header, producerValidator, cfg)
			switch status {
			case http.StatusOK:
				claims = promoted
			case http.StatusForbidden:
				c.AbortWithStatus(http.StatusForbidden)
				return
			default:
				// An unavailable validator is 503 (ADR 0014); refusals keep
				// the worker validator's fixed message.
				abortAuthFailure(c, auth.PreferUnavailable(err, promotionErr))
				return
			}
		}
		if !authorizeBindingScope(c, claims) {
			return
		}
		c.Set("workerClaims", claims)

		tenantID, err := extractTenantID(claims)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{tenantClaimsErrorKey: tenantClaimsErrorMessage})
			return
		}
		c.Set("tenantID", tenantID)

		c.Next()
	}
}

func validateWorkerShape(claims *auth.Claims) error {
	if len(claims.EventTypes) == 0 {
		return errMissingEventTypes
	}
	if len(claims.Scopes) == 0 {
		return errMissingScope
	}
	return nil
}

// promoteProducerToWorker implements the ALLOW_PRODUCER_AS_WORKER
// compatibility mode. It returns http.StatusOK with promoted claims,
// http.StatusForbidden for a topic-controller token, or
// http.StatusUnauthorized when promotion is unavailable, together with the
// producer validator's error (nil when promotion was not attempted).
// Binding-scoped tokens and any token carrying codeq:publish are never
// promoted (platform ADR-0022 C1.1).
func promoteProducerToWorker(c *gin.Context, header string, producerValidator auth.Validator, cfg *config.Config) (*auth.Claims, int, error) {
	if cfg == nil || !cfg.AllowProducerAsWorker || producerValidator == nil {
		return nil, http.StatusUnauthorized, nil
	}
	pclaims, err := validateBearer(producerValidator, header)
	if err != nil {
		return nil, http.StatusUnauthorized, err
	}
	if pclaims.HasScope(topicManageScope) {
		return nil, http.StatusForbidden, nil
	}
	if authclaims.RequiresBindingScope(pclaims) {
		RecordBindingDenial("producer_promotion", routeLabel(c))
		return nil, http.StatusUnauthorized, nil
	}
	return &auth.Claims{
		Subject:    pclaims.Subject,
		Email:      pclaims.Email,
		Issuer:     "producer",
		Audience:   []string{cfg.WorkerAudience},
		ExpiresAt:  pclaims.ExpiresAt,
		IssuedAt:   pclaims.IssuedAt,
		Scopes:     append([]string(nil), promotedWorkerScopes...),
		EventTypes: []string{"*"},
		Raw:        pclaims.Raw,
	}, http.StatusOK, nil
}

func GetWorkerClaims(c *gin.Context) (*auth.Claims, bool) {
	v, ok := c.Get("workerClaims")
	if !ok {
		return nil, false
	}
	claims, ok := v.(*auth.Claims)
	return claims, ok
}
