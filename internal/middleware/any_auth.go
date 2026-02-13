package middleware

import (
	"net/http"

	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

func AnyAuthMiddleware(cfg *config.Config) gin.HandlerFunc {
	workerValidator, werr := newWorkerValidator(cfg)
	producerValidator, _ := newProducerValidator(cfg)
	return func(c *gin.Context) {
		if werr == nil && workerValidator != nil {
			claims, err := validateBearer(workerValidator, c.GetHeader("Authorization"))
			if err == nil && len(claims.EventTypes) > 0 {
				c.Set("workerClaims", claims)
				c.Set("authType", "worker")
				c.Next()
				return
			}
		}

		if producerValidator != nil {
			claims, err := validateBearer(producerValidator, c.GetHeader("Authorization"))
			if err == nil {
				setProducerContext(c, cfg, claims)
				c.Set("authType", "producer")
				c.Next()
				return
			}
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
	}
}
