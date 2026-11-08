package jwks

import (
	"slices"
	"sync"
	"testing"
	"time"
)

type recordingObserver struct {
	mu       sync.Mutex
	outcomes []string
	skipped  []string
	states   []string
}

func (r *recordingObserver) RefreshCompleted(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

func (r *recordingObserver) RefreshSkipped(trigger string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skipped = append(r.skipped, trigger)
}

func (r *recordingObserver) KeyLookup(state string, age time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state == decisionUnavailable && age != 0 {
		panic("unavailable lookups must not report an age")
	}
	r.states = append(r.states, state)
}

func TestObserverReceivesOnlyFixedValues(t *testing.T) {
	rec := &recordingObserver{}
	SetObserver(rec)
	t.Cleanup(func() { SetObserver(nil) })

	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveFailure)
	requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	h.clock.Advance(h.v.minRefreshInterval)
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("validate: %v", err)
	}
	requireInvalid(t, mustErr(h.v.Validate(h.token(t, keys[1], "unknown"))))
	h.clock.Advance(h.v.refreshInterval)
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("stale validate: %v", err)
	}
	h.settle(t)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if want := []string{outcomeHTTPStatus, outcomeSuccess, outcomeSuccess}; !slices.Equal(rec.outcomes, want) {
		t.Fatalf("outcomes %v, want %v", rec.outcomes, want)
	}
	if want := []string{"unknown_kid"}; !slices.Equal(rec.skipped, want) {
		t.Fatalf("skipped %v, want %v", rec.skipped, want)
	}
	if want := []string{decisionUnavailable, decisionFresh, decisionFresh, decisionStale}; !slices.Equal(rec.states, want) {
		t.Fatalf("states %v, want %v", rec.states, want)
	}
}

func TestNoopObserverIsDefault(t *testing.T) {
	SetObserver(nil)
	o := observer()
	o.RefreshCompleted(outcomeSuccess)
	o.RefreshSkipped("unknown_kid")
	o.KeyLookup(decisionFresh, time.Second)
	if _, ok := o.(noopObserver); !ok {
		t.Fatalf("default observer is %T", o)
	}
}
