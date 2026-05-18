package controllers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// respondWriteError writes the right response for a service-level
// error from a WRITE controller (create, claim, submit, nack, etc.).
// If the error indicates "not leader" with a known leader URL, replies
// with HTTP 307 Temporary Redirect so a well-behaved HTTP client
// follows to the leader transparently. Otherwise falls back to the
// usual 400.
//
// HTTP 307 (vs the older 302) preserves the request method and body
// across the redirect (RFC 7231), which is what we want for POST /tasks
// and friends. Go's http.Client follows automatically when the
// request body supports Seek (bytes.Reader / strings.Reader / GetBody).
func respondWriteError(c *gin.Context, err error) {
	if maybeRedirectLeader(c, err) {
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

// maybeRedirectLeader writes a 307 to the leader and returns true when
// err is a "not leader" hint with a known leader URL. Otherwise it
// returns false and writes nothing.
func maybeRedirectLeader(c *gin.Context, err error) bool {
	var hint domain.LeaderHint
	if !errors.As(err, &hint) {
		return false
	}
	addr := hint.LeaderHTTPAddr()
	if addr == "" {
		return false
	}
	location := strings.TrimSuffix(addr, "/") + c.Request.URL.RequestURI()
	c.Header("Location", location)
	c.JSON(http.StatusTemporaryRedirect, gin.H{
		"error":  "not leader",
		"leader": addr,
	})
	return true
}
