package middleware

import (
	"errors"
	"net/http"
	"time"

	identitymw "github.com/codecompany/identity-middleware/pkg/identitymiddleware"
	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

func WorkerAuthMiddleware(cfg *config.Config) gin.HandlerFunc {
	workerValidator, werr := newWorkerValidator(cfg)
	producerValidator, _ := newProducerValidator(cfg)
	if werr != nil {
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
					claims = &identitymw.Claims{
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

func newWorkerValidator(cfg *config.Config) (*identitymw.Validator, error) {
	return identitymw.NewValidator(identitymw.Config{
		JwksURL:     cfg.WorkerJwksURL,
		Issuer:      cfg.WorkerIssuer,
		Audience:    cfg.WorkerAudience,
		ClockSkew:   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
		HTTPTimeout: 5 * time.Second,
	})
}

func GetWorkerClaims(c *gin.Context) (*identitymw.Claims, bool) {
	v, ok := c.Get("workerClaims")
	if !ok {
		return nil, false
	}
	claims, ok := v.(*identitymw.Claims)
	return claims, ok
}
