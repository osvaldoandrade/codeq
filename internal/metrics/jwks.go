package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/osvaldoandrade/codeq/pkg/auth/jwks"
)

// labelOutcome is the fixed refresh-outcome label.
const labelOutcome = "outcome"

// JWKS validator metrics (ADR 0014). Every label value comes from the fixed
// sets documented on jwks.Observer; no token data, URL or error text.
var (
	JWKSRefreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "jwks_refresh_total",
		Help:      "JWKS refresh attempts by fixed outcome.",
	}, []string{labelOutcome})

	JWKSRefreshSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "jwks_refresh_skipped_total",
		Help:      "JWKS refreshes not started because the minimum refresh interval had not elapsed, by trigger.",
	}, []string{"trigger"})

	JWKSKeySetDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "jwks_key_set_decisions_total",
		Help:      "Token key lookups by key-set state (fresh, stale, unavailable, kid_not_yet_known, canceled).",
	}, []string{"state"})

	JWKSKeySetAgeSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "jwks_key_set_age_seconds",
		Help:      "Age of the JWKS key set used for a token key lookup (fresh or stale).",
		Buckets:   []float64{30, 60, 120, 300, 360, 600, 900},
	})
)

// jwksObserver exports jwks.Observer events as Prometheus metrics.
type jwksObserver struct{}

// RefreshCompleted counts one finished refresh attempt by outcome.
func (jwksObserver) RefreshCompleted(outcome string) {
	JWKSRefreshTotal.WithLabelValues(outcome).Inc()
}

// RefreshSkipped counts one refresh suppressed by the minimum interval.
func (jwksObserver) RefreshSkipped(trigger string) {
	JWKSRefreshSkipped.WithLabelValues(trigger).Inc()
}

// KeyLookup counts one key lookup by key-set state and observes its age.
func (jwksObserver) KeyLookup(state string, keySetAge time.Duration) {
	JWKSKeySetDecisions.WithLabelValues(state).Inc()
	if keySetAge > 0 {
		JWKSKeySetAgeSeconds.Observe(keySetAge.Seconds())
	}
}

func init() {
	prometheus.MustRegister(JWKSRefreshTotal, JWKSRefreshSkipped, JWKSKeySetDecisions, JWKSKeySetAgeSeconds)
	jwks.SetObserver(jwksObserver{})
}
