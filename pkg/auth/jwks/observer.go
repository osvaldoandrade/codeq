package jwks

import (
	"sync/atomic"
	"time"
)

// Refresh outcomes. These fixed values are the only refresh detail that
// reaches observers or logs: never the JWKS URL, transport error text or any
// token content.
const (
	outcomeSuccess         = "success"
	outcomeNoUsableKeys    = "no_usable_keys"
	outcomeFetchTimeout    = "fetch_timeout"
	outcomeFetchError      = "fetch_error"
	outcomeHTTPStatus      = "http_status"
	outcomeTooLarge        = "too_large"
	outcomeInvalidDocument = "invalid_document"
	outcomeDuplicateKid    = "duplicate_kid"
)

// Key-set states reported per key lookup.
const (
	decisionFresh       = "fresh"       // set younger than the refresh interval
	decisionStale       = "stale"       // older than the refresh interval, within the bound
	decisionUnavailable = "unavailable" // no usable set: the request is answered 503
)

// Observer receives validator events whose string arguments come from fixed
// sets, so they are safe as metric labels:
//
//   - RefreshCompleted outcome: success, no_usable_keys, fetch_timeout,
//     fetch_error, http_status, too_large, invalid_document, duplicate_kid.
//   - RefreshSkipped trigger: unknown_kid, refresh_ahead.
//   - KeyLookup state: fresh, stale, unavailable; keySetAge is zero for
//     unavailable.
//
// Implementations must be cheap and must not block.
type Observer interface {
	RefreshCompleted(outcome string)
	RefreshSkipped(trigger string)
	KeyLookup(state string, keySetAge time.Duration)
}

type noopObserver struct{}

// RefreshCompleted ignores the event.
func (noopObserver) RefreshCompleted(string) {}

// RefreshSkipped ignores the event.
func (noopObserver) RefreshSkipped(string) {}

// KeyLookup ignores the event.
func (noopObserver) KeyLookup(string, time.Duration) {}

type observerHolder struct{ Observer }

var currentObserver atomic.Pointer[observerHolder]

// SetObserver installs the process-wide observer for every JWKS validator;
// nil restores the no-op observer. The server installs a Prometheus observer
// from internal/metrics.
func SetObserver(o Observer) {
	if o == nil {
		o = noopObserver{}
	}
	currentObserver.Store(&observerHolder{o})
}

func observer() Observer {
	if h := currentObserver.Load(); h != nil {
		return h.Observer
	}
	return noopObserver{}
}

func stateFor(age, refreshInterval time.Duration) string {
	if age < refreshInterval {
		return decisionFresh
	}
	return decisionStale
}

func observeDecision(state string, age time.Duration) {
	if state == decisionUnavailable {
		age = 0
	}
	observer().KeyLookup(state, age)
}
