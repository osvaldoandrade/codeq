package middleware

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/auth/jwks"
	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

const adminScope = "codeq:admin"

func AuthMiddleware(cfg *config.Config) gin.HandlerFunc {
	validator, err := newProducerValidator(cfg)
	if err != nil {
		return func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "identity validator not configured"})
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

func newProducerValidator(cfg *config.Config) (auth.Validator, error) {
	return jwks.NewValidator(auth.Config{
		JwksURL:     cfg.IdentityJwksURL,
		Issuer:      cfg.IdentityIssuer,
		Audience:    cfg.IdentityAudience,
		ClockSkew:   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
		HTTPTimeout: 5 * time.Second,
	})
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
}
