package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	jwksDecisionsName = "codeq_jwks_key_set_decisions_total"
	stateStale        = "stale"
	stateUnavailable  = "unavailable"
)

// gathered returns the counter value of family name with label value label,
// or the histogram sample count when the family is a histogram.
func gathered(t *testing.T, name, label string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if h := metric.GetHistogram(); h != nil {
				return float64(h.GetSampleCount())
			}
			for _, l := range metric.GetLabel() {
				if l.GetValue() == label {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func TestJWKSObserverRecordsFixedLabels(t *testing.T) {
	o := jwksObserver{}
	before := gathered(t, "codeq_jwks_refresh_total", "fetch_timeout")
	o.RefreshCompleted("fetch_timeout")
	if got := gathered(t, "codeq_jwks_refresh_total", "fetch_timeout") - before; got != 1 {
		t.Fatalf("refresh counter delta %v", got)
	}
	skipped := gathered(t, "codeq_jwks_refresh_skipped_total", "unknown_kid")
	o.RefreshSkipped("unknown_kid")
	if got := gathered(t, "codeq_jwks_refresh_skipped_total", "unknown_kid") - skipped; got != 1 {
		t.Fatalf("skipped counter delta %v", got)
	}
	stale := gathered(t, jwksDecisionsName, stateStale)
	unavailable := gathered(t, jwksDecisionsName, stateUnavailable)
	samples := gathered(t, "codeq_jwks_key_set_age_seconds", "")
	o.KeyLookup(stateStale, 400*time.Second)
	o.KeyLookup(stateUnavailable, 0)
	if gathered(t, jwksDecisionsName, stateStale)-stale != 1 ||
		gathered(t, jwksDecisionsName, stateUnavailable)-unavailable != 1 {
		t.Fatal("decision counters not incremented")
	}
	if got := gathered(t, "codeq_jwks_key_set_age_seconds", "") - samples; got != 1 {
		t.Fatalf("age histogram observed %v samples, want 1 (unavailable has no age)", got)
	}
}
