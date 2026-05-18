package controllers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/osvaldoandrade/codeq/internal/services"

	"github.com/gin-gonic/gin"
)

const (
	// maxLongPollSeconds caps `?waitSeconds=` to keep connection budgets
	// predictable. Producers that want to wait longer should retry.
	maxLongPollSeconds = 60
	// longPollInterval is how often the controller re-checks the result
	// while waiting. Pebble's `Get` is a memtable/L0 lookup so 100 ms is
	// effectively free; tighter intervals add CPU without changing
	// perceived latency.
	longPollInterval = 100 * time.Millisecond
)

type getResultController struct{ svc services.ResultsService }

func NewGetResultController(s services.ResultsService) *getResultController {
	return &getResultController{svc: s}
}

// Handle returns the stored result for a task. With `?waitSeconds=N`
// the request is held open until either the result becomes available
// or the deadline elapses (long-poll). Without the query param the
// behavior is the original fire-and-return.
func (h *getResultController) Handle(c *gin.Context) {
	id := c.Param("id")
	wait := parseWaitSeconds(c.Query("waitSeconds"))

	if rec, task, err := h.svc.Get(c.Request.Context(), id); err == nil {
		c.JSON(http.StatusOK, gin.H{"task": task, "result": rec})
		return
	} else if wait == 0 || err.Error() == "task not found" {
		// No long-poll requested, or the task itself doesn't exist —
		// no amount of waiting will make a result appear.
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(longPollInterval)
	defer tick.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			// Client gave up; nothing left to send.
			return
		case <-deadline.C:
			rec, task, err := h.svc.Get(c.Request.Context(), id)
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"task": task, "result": rec})
			return
		case <-tick.C:
			rec, task, err := h.svc.Get(c.Request.Context(), id)
			if err != nil {
				continue
			}
			c.JSON(http.StatusOK, gin.H{"task": task, "result": rec})
			return
		}
	}
}

func parseWaitSeconds(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	if n > maxLongPollSeconds {
		return maxLongPollSeconds
	}
	return n
}
