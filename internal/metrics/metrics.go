package metrics

import "github.com/prometheus/client_golang/prometheus"

const (
	namespace = "codeq"
	// labelRoute is the bounded route-template label shared by several vectors.
	labelRoute = "route"
)

var (
	TaskCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "task_created_total",
			Help:      "Total number of tasks created (enqueued).",
		},
		[]string{"command"},
	)

	TaskClaimedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "task_claimed_total",
			Help:      "Total number of tasks claimed by workers.",
		},
		[]string{"command"},
	)

	TaskCompletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "task_completed_total",
			Help:      "Total number of tasks completed, labeled by final status.",
		},
		[]string{"command", "status"},
	)

	TaskProcessingLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "task_processing_latency_seconds",
			Help:      "End-to-end latency from task creation to completion (seconds).",
			Buckets:   []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600},
		},
		[]string{"command", "status"},
	)

	LeaseExpiredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "lease_expired_total",
			Help:      "Total number of lease expirations detected during claim-time repair.",
		},
		[]string{"command"},
	)

	WebhookDeliveriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "webhook_deliveries_total",
			Help:      "Total number of webhook deliveries, labeled by kind and outcome.",
		},
		[]string{"kind", "command", "outcome"},
	)

	RateLimitHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rate_limit_hits_total",
			Help:      "Total number of rate limit rejections, labeled by scope and operation.",
		},
		[]string{"scope", "operation"},
	)

	// BindingScopeDeniedTotal counts refused binding-scoped token requests
	// (platform ADR-0022). Labels are bounded: reason is a fixed code set and
	// route is a registered route template or a fixed gRPC stream name.
	BindingScopeDeniedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "binding_scope_denied_total",
			Help:      "Total number of refused binding-scoped token requests, labeled by reason and route.",
		},
		[]string{"reason", labelRoute},
	)

	// LeaderForwardTotal counts follower-to-leader forwarding decisions
	// (platform ADR-0022 C1.5). Labels are bounded: route is a registered
	// route template and result is a fixed set defined by leaderforward.
	LeaderForwardTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "leader_forward_total",
			Help:      "Total number of follower-to-leader request forwards, labeled by route and result.",
		},
		[]string{labelRoute, "result"},
	)

	// IdempotencyConflictTotal counts create requests refused with 409
	// idempotency_conflict because the key already maps to a task the caller
	// may not replay (another tenant, or another topic for a binding token).
	// route is a registered route template; kind is "binding" or "token".
	// QueueDepth counts tasks observed in a named queue. The Pebble
	// repository updates it when a task enters the ready queue.
	QueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "queue_depth",
			Help:      "Tasks observed in a queue.",
		},
		[]string{"command", "queue"},
	)

	IdempotencyConflictTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "idempotency_conflict_total",
			Help:      "Total number of create requests refused because the idempotency key belongs to another tenant or topic.",
		},
		[]string{labelRoute, "kind"},
	)

	// ScheduleFiresTotal counts recurring schedule slots by outcome:
	// enqueued (task created or replayed and the slot recorded), failed
	// (the create failed; retried on the next tick) or superseded (the spec
	// changed while firing; the next tick follows the new spec).
	ScheduleFiresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "schedule_fires_total",
			Help:      "Total number of recurring schedule slots fired, labeled by outcome.",
		},
		[]string{"outcome"},
	)

	// ScheduleFireDelaySeconds is how late a slot was enqueued relative to
	// its scheduled time.
	ScheduleFireDelaySeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "schedule_fire_delay_seconds",
			Help:      "Delay between a recurring schedule slot and the enqueue of its task.",
			Buckets:   []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 300},
		},
	)
)

func init() {
	prometheus.MustRegister(
		TaskCreatedTotal,
		TaskClaimedTotal,
		TaskCompletedTotal,
		TaskProcessingLatencySeconds,
		LeaseExpiredTotal,
		WebhookDeliveriesTotal,
		RateLimitHitsTotal,
		BindingScopeDeniedTotal,
		LeaderForwardTotal,
		IdempotencyConflictTotal,
		QueueDepth,
		ScheduleFiresTotal,
		ScheduleFireDelaySeconds,
	)
}
