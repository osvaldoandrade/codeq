package middleware

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

const adminScope = "codeq:admin"

// AuthMiddleware creates producer authentication middleware with the provided validator
func AuthMiddleware(validator auth.Validator, cfg *config.Config) gin.HandlerFunc {
	if validator == nil {
		return func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "producer validator not configured"})
		}
	}
	return func(c *gin.Context) {
		claims, err := validateBearer(validator, c.GetHeader("Authorization"))
		if err != nil {
			abortAuthFailure(c, err)
			return
		}
		if !authorizeBindingScope(c, claims) {
			return
		}
		tenantID, err := extractTenantID(claims)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{tenantClaimsErrorKey: tenantClaimsErrorMessage})
			return
		}
		if claims.HasScope(topicManageScope) && !authorizeTopicController(c, cfg, claims, tenantID) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		setProducerContext(c, cfg, claims, tenantID)
		c.Next()
	}
}

// Fixed authentication failure messages. Only these sentinels are echoed in
// a 401 body; every other validator error becomes msgInvalidToken so the body
// never carries verifier internals (JWKS URL, dial errors, issuer values).
var (
	errMissingAuthorization = errors.New("missing Authorization header")
	errInvalidAuthorization = errors.New("invalid Authorization format")
	errMissingEventTypes    = errors.New("missing eventTypes")
	errMissingScope         = errors.New("missing scope")
)

const (
	msgInvalidToken    = "invalid token"
	msgAuthUnavailable = "authentication temporarily unavailable"
)

func validateBearer(validator auth.Validator, authHeader string) (*auth.Claims, error) {
	if strings.TrimSpace(authHeader) == "" {
		return nil, errMissingAuthorization
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, errInvalidAuthorization
	}
	return validator.Validate(parts[1])
}

// abortAuthFailure answers 503 with Retry-After when the token could not be
// evaluated (no usable key set, ADR 0014) and 401 with a fixed body when it
// was refused. Neither path accepts the request.
func abortAuthFailure(c *gin.Context, err error) {
	if retry, ok := auth.RetryAfter(err); ok {
		c.Header("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{tenantClaimsErrorKey: msgAuthUnavailable})
		return
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{tenantClaimsErrorKey: publicAuthMessage(err)})
}

func publicAuthMessage(err error) string {
	for _, fixed := range []error{errMissingAuthorization, errInvalidAuthorization, errMissingEventTypes, errMissingScope} {
		if errors.Is(err, fixed) {
			return fixed.Error()
		}
	}
	return msgInvalidToken
}

func setProducerContext(c *gin.Context, cfg *config.Config, claims *auth.Claims, tenantID string) {
	c.Set("userClaims", claims)
	email := strings.TrimSpace(claims.Email)
	if email == "" {
		email = strings.TrimSpace(claims.Subject)
	}
	c.Set("userEmail", email)

	role := ""
	if v, ok := claims.Raw["role"].(string); ok {
		role = strings.ToUpper(strings.TrimSpace(v))
	}
	// Binding-scoped tokens never acquire a role from a request header.
	if role == "" && cfg.Env == "dev" && !authclaims.RequiresBindingScope(claims) {
		role = strings.ToUpper(strings.TrimSpace(c.GetHeader("X-Role")))
	}
	if role == "" {
		role = "USER"
	}
	c.Set("userRole", role)

	c.Set("tenantID", tenantID)
}
