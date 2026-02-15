package middleware

import (
	"fmt"
	"net/http"
	"strings"

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
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		setProducerContext(c, cfg, claims)
		c.Next()
	}
}

func validateBearer(validator auth.Validator, authHeader string) (*auth.Claims, error) {
	if strings.TrimSpace(authHeader) == "" {
		return nil, fmt.Errorf("missing Authorization header")
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, fmt.Errorf("invalid Authorization format")
	}
	return validator.Validate(parts[1])
}

func setProducerContext(c *gin.Context, cfg *config.Config, claims *auth.Claims) {
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
	if role == "" && cfg.Env == "dev" {
		role = strings.ToUpper(strings.TrimSpace(c.GetHeader("X-Role")))
	}
	if role == "" {
		role = "USER"
	}
	c.Set("userRole", role)

	// Extract tenant ID from JWT claims
	tenantID := extractTenantID(claims)
	c.Set("tenantID", tenantID)
}
