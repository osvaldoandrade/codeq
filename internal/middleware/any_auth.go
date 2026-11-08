package middleware

import (
	"context"
	"errors"
	"net/http"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

// AnyAuthMiddleware creates middleware that accepts either worker or producer tokens
func AnyAuthMiddleware(workerValidator, producerValidator auth.Validator, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		claims, workerErr := validateWith(c.Request.Context(), workerValidator, header)
		if workerErr == nil && claims != nil {
			if claims.HasScope(topicManageScope) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			// A binding candidate accepted by the worker validator is judged
			// here so an invalid one is refused instead of retried as producer.
			if len(claims.EventTypes) > 0 || authclaims.RequiresBindingScope(claims) {
				anyAuthWorker(c, claims)
				return
			}
		}

		claims, producerErr := validateWith(c.Request.Context(), producerValidator, header)
		if producerErr == nil && claims != nil {
			if claims.HasScope(topicManageScope) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			anyAuthProducer(c, cfg, claims)
			return
		}

		// Unavailability of either validator is 503, never a refusal.
		abortAuthFailure(c, auth.PreferUnavailable(workerErr, producerErr))
	}
}

// validateWith returns errNoValidator for an absent validator so a nil
// validator never reads as an acceptance.
func validateWith(ctx context.Context, validator auth.Validator, header string) (*auth.Claims, error) {
	if validator == nil {
		return nil, errNoValidator
	}
	claims, err := validateBearer(ctx, validator, header)
	if err == nil && claims == nil {
		return nil, errNoValidator
	}
	return claims, err
}

var errNoValidator = errors.New("validator not configured")

func anyAuthWorker(c *gin.Context, claims *auth.Claims) {
	if !authorizeBindingScope(c, claims) {
		return
	}
	tenantID, err := extractTenantID(claims)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{tenantClaimsErrorKey: tenantClaimsErrorMessage})
		return
	}
	c.Set("workerClaims", claims)
	c.Set("tenantID", tenantID)
	c.Set("authType", "worker")
	c.Next()
}

func anyAuthProducer(c *gin.Context, cfg *config.Config, claims *auth.Claims) {
	if !authorizeBindingScope(c, claims) {
		return
	}
	tenantID, err := extractTenantID(claims)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{tenantClaimsErrorKey: tenantClaimsErrorMessage})
		return
	}
	setProducerContext(c, cfg, claims, tenantID)
	c.Set("authType", "producer")
	c.Next()
}
