package controllers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/osvaldoandrade/codeq/internal/leaderforward"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// respondWriteError writes the right response for a service-level error
// from a WRITE controller (create, claim, submit, nack, etc.). A Raft
// "not leader" error is forwarded to the configured leader in-process
// (platform ADR-0022 C1.5) or answered with 503 leader_unavailable; no
// client ever receives a 307. Every other error keeps the usual 400.
func respondWriteError(c *gin.Context, err error) {
	if maybeForwardLeader(c, err) {
		return
	}
	if errors.Is(err, domain.ErrStorageCapacity) {
		respondStorageCapacity(c)
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

// maybeForwardLeader handles a Raft "not leader" error and returns true
// when it wrote the response. Otherwise it returns false and writes nothing.
func maybeForwardLeader(c *gin.Context, err error) bool {
	return leaderforward.HandleNotLeader(c, err)
}

// storageCapacityRetryAfter is a hint, not a promise: space returns as
// workers drain the queue and compaction/cleanup reclaim it.
const storageCapacityRetryAfter = "30"

// respondStorageCapacity answers a create refused by the data-volume guard
// with a stable code. It never includes paths or volume figures.
func respondStorageCapacity(c *gin.Context) {
	c.Header("Retry-After", storageCapacityRetryAfter)
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error":   domain.StorageCapacityCode,
		"message": "Queue storage is above its admission threshold. New tasks are refused while existing tasks drain; retry later.",
	})
}
