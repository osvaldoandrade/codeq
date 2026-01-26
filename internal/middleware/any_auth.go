package middleware

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

func AnyAuthMiddleware(cfg *config.Config) gin.HandlerFunc {
	cache := &jwksCache{}
	return func(c *gin.Context) {
		// try worker first
		claims, werr := validateWorkerJWT(c.Request.Context(), cfg, cache, c.GetHeader("Authorization"))
		if werr == nil {
			c.Set("workerClaims", claims)
			c.Set("authType", "worker")
			c.Next()
			return
		}

		user, perr := validateProducerToken(c.Request.Context(), cfg, c.GetHeader("Authorization"))
		if perr == nil {
			c.Set("userEmail", user.Email)
			role := c.GetHeader("X-Role")
			if role == "" {
				role = "USER"
			}
			c.Set("userRole", role)
			c.Set("authType", "producer")
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
	}
}
