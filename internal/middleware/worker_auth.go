package middleware

import (
	"errors"
	"net/http"

	identitymw "github.com/osvaldoandrade/codeq/internal/identitymw"
	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

// WorkerAuthMiddleware creates worker authentication middleware with the provided validators
func WorkerAuthMiddleware(workerValidator, producerValidator auth.Validator, cfg *config.Config) gin.HandlerFunc {
	if workerValidator == nil {
		return func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "worker validator not configured"})
		}
	}

	return func(c *gin.Context) {
		claims, err := validateBearer(workerValidator, c.GetHeader("Authorization"))
		if err == nil {
			if len(claims.EventTypes) == 0 {
				err = errors.New("missing eventTypes")
			} else if len(claims.Scopes) == 0 {
				err = errors.New("missing scope")
			}
		}
		if err != nil {
			if cfg.AllowProducerAsWorker && producerValidator != nil {
				pclaims, perr := validateBearer(producerValidator, c.GetHeader("Authorization"))
				if perr == nil {
					claims = &auth.Claims{
						Subject:    pclaims.Subject,
						Email:      pclaims.Email,
						Issuer:     "producer",
						Audience:   []string{cfg.WorkerAudience},
						ExpiresAt:  pclaims.ExpiresAt,
						IssuedAt:   pclaims.IssuedAt,
						Scopes:     []string{"codeq:claim", "codeq:heartbeat", "codeq:abandon", "codeq:nack", "codeq:result", "codeq:subscribe"},
						EventTypes: []string{"*"},
						Raw:        pclaims.Raw,
					}
				} else {
					c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
					return
				}
			} else {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
				return
			}
		}
		c.Set("workerClaims", claims)
		c.Next()
	}
}

func GetWorkerClaims(c *gin.Context) (*auth.Claims, bool) {
	v, ok := c.Get("workerClaims")
	if !ok {
		return nil, false
	}
	claims, ok := v.(*auth.Claims)
	return claims, ok
}
