package metrics

import "github.com/prometheus/client_golang/prometheus"

const namespace = "codeq"

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
)

func init() {
	prometheus.MustRegister(
		TaskCreatedTotal,
		TaskClaimedTotal,
		TaskCompletedTotal,
		TaskProcessingLatencySeconds,
		LeaseExpiredTotal,
		WebhookDeliveriesTotal,
	)
}
