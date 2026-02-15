package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/prometheus/client_golang/prometheus"
)

type redisCollector struct {
	rdb    *redis.Client
	logger *slog.Logger

	queueDepthDesc *prometheus.Desc
	dlqDepthDesc   *prometheus.Desc
	subsActiveDesc *prometheus.Desc
}

func newRedisCollector(rdb *redis.Client, logger *slog.Logger) *redisCollector {
	if logger == nil {
		logger = slog.Default()
	}
	return &redisCollector{
		rdb:    rdb,
		logger: logger,
		queueDepthDesc: prometheus.NewDesc(
			"codeq_queue_depth",
			"Current queue depth by command and queue state.",
			[]string{"command", "queue"},
			nil,
		),
		dlqDepthDesc: prometheus.NewDesc(
			"codeq_dlq_depth",
			"Current DLQ depth by command.",
			[]string{"command"},
			nil,
		),
		subsActiveDesc: prometheus.NewDesc(
			"codeq_subscriptions_active",
			"Current active subscriptions by command (eventType).",
			[]string{"command"},
			nil,
		),
	}
}

func (c *redisCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.queueDepthDesc
	ch <- c.dlqDepthDesc
	ch <- c.subsActiveDesc
}

func (c *redisCollector) Collect(ch chan<- prometheus.Metric) {
	if c.rdb == nil {
		return
	}

	// Keep Redis reads bounded so scrapes do not hang.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	commands := []domain.Command{domain.CmdGenerateMaster, domain.CmdGenerateCreative}
	nowUnix := time.Now().UTC().Unix()
	minScore := strconv.FormatInt(nowUnix, 10)

	pipe := c.rdb.Pipeline()
	readyCmds := make(map[domain.Command][]*redis.IntCmd, len(commands))
	inprogCmds := make(map[domain.Command]*redis.IntCmd, len(commands))
	delayedCmds := make(map[domain.Command]*redis.IntCmd, len(commands))
	dlqCmds := make(map[domain.Command]*redis.IntCmd, len(commands))
	subsCmds := make(map[domain.Command]*redis.IntCmd, len(commands))

	for _, cmd := range commands {
		var llens []*redis.IntCmd
		for p := 0; p <= 9; p++ {
			llens = append(llens, pipe.LLen(ctx, keyQueuePending(cmd, p)))
		}
		readyCmds[cmd] = llens
		inprogCmds[cmd] = pipe.SCard(ctx, keyQueueInprog(cmd))
		delayedCmds[cmd] = pipe.ZCard(ctx, keyQueueDelayed(cmd))
		dlqCmds[cmd] = pipe.LLen(ctx, keyQueueDLQ(cmd))
		subsCmds[cmd] = pipe.ZCount(ctx, keySubsEvent(cmd), minScore, "+inf")
	}

	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		c.logger.Warn("prometheus redis collector failed", "err", err)
		return
	}

	for _, cmd := range commands {
		var ready int64
		for _, ccmd := range readyCmds[cmd] {
			ready += ccmd.Val()
		}
		inprog := inprogCmds[cmd].Val()
		delayed := delayedCmds[cmd].Val()
		dlq := dlqCmds[cmd].Val()
		subsActive := subsCmds[cmd].Val()

		emitGauge(ch, c.queueDepthDesc, float64(ready), string(cmd), "ready")
		emitGauge(ch, c.queueDepthDesc, float64(delayed), string(cmd), "delayed")
		emitGauge(ch, c.queueDepthDesc, float64(inprog), string(cmd), "in_progress")
		emitGauge(ch, c.queueDepthDesc, float64(dlq), string(cmd), "dlq")
		emitGauge(ch, c.dlqDepthDesc, float64(dlq), string(cmd))
		emitGauge(ch, c.subsActiveDesc, float64(subsActive), string(cmd))
	}
}

func emitGauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, v float64, labelValues ...string) {
	m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, v, labelValues...)
	if err != nil {
		return
	}
	ch <- m
}

func keyQueuePending(cmd domain.Command, priority int) string {
	return fmt.Sprintf("codeq:q:%s:pending:%d", strings.ToLower(string(cmd)), priority)
}

func keyQueueInprog(cmd domain.Command) string {
	return fmt.Sprintf("codeq:q:%s:inprog", strings.ToLower(string(cmd)))
}

func keyQueueDelayed(cmd domain.Command) string {
	return fmt.Sprintf("codeq:q:%s:delayed", strings.ToLower(string(cmd)))
}

func keyQueueDLQ(cmd domain.Command) string {
	return fmt.Sprintf("codeq:q:%s:dlq", strings.ToLower(string(cmd)))
}

func keySubsEvent(cmd domain.Command) string {
	return fmt.Sprintf("codeq:subs:%s", strings.ToLower(string(cmd)))
}

var registerRedisCollectorOnce sync.Once

func RegisterRedisCollector(rdb *redis.Client, logger *slog.Logger) {
	registerRedisCollectorOnce.Do(func() {
		prometheus.MustRegister(newRedisCollector(rdb, logger))
	})
}
