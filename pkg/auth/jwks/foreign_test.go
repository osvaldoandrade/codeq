package jwks

import (
	"crypto/rsa"
	"testing"
	"time"
)

// foreignTokens are tokens signed with key for kid whose unverified claims
// name another issuer or audience, or none.
func (h *testHarness) foreignTokens(t *testing.T, key *rsa.PrivateKey, kid string) map[string]string {
	t.Helper()
	iat := h.clock.Now()
	return map[string]string{
		"other issuer":   signToken(t, key, kid, claimsAt("https://other-issuer", testAudience, iat)),
		"other audience": signToken(t, key, kid, claimsAt(testIssuer, "other-audience", iat)),
		"no audience":    signToken(t, key, kid, claimsAt(testIssuer, []string{}, iat)),
		"no issuer":      signToken(t, key, kid, claimsAt("", testAudience, iat)),
	}
}

// requireForeignRefused checks that every foreign token is 401 without a
// JWKS fetch or a refresh left in flight.
func (h *testHarness) requireForeignRefused(t *testing.T, key *rsa.PrivateKey, kid string) {
	t.Helper()
	before := h.jwks.fetches.Load()
	for name, token := range h.foreignTokens(t, key, kid) {
		started := time.Now()
		requireInvalid(t, mustErr(h.v.Validate(token)))
		if elapsed := time.Since(started); elapsed > h.v.httpTimeout {
			t.Fatalf("%s: refusal waited %v", name, elapsed)
		}
	}
	if got := h.jwks.fetches.Load() - before; got != 0 {
		t.Fatalf("foreign tokens drove %d fetches", got)
	}
	h.v.mu.Lock()
	inflight := h.v.inflight
	h.v.mu.Unlock()
	if inflight != nil {
		t.Fatal("a foreign token started a refresh")
	}
}

// Security review L-1: every "could not check" path answers 503 only for a
// token that names this validator's issuer and audience; a token for another
// issuer or audience is 401 at once on each of them.
func TestForeignTokenIsRefusedOnEveryUnavailablePath(t *testing.T) {
	keys := testKeys(t)

	t.Run("no usable key set", func(t *testing.T) {
		h := newHarness(t)
		h.jwks.set(serveFailure)
		h.requireForeignRefused(t, keys[0], "k1")
		// Our token waits for the cold-start refresh, which fails (no set
		// after the wait), then meets the failure inside the interval.
		requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
		requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
		h.requireForeignRefused(t, keys[0], "k1")
	})

	t.Run("set past the age bound", func(t *testing.T) {
		h := newHarness(t)
		h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
		if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
			t.Fatalf("warm-up: %v", err)
		}
		h.jwks.set(serveFailure)
		h.clock.Advance(h.v.maxKeySetAge + time.Second)
		h.requireForeignRefused(t, keys[0], "k1")
		requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
		h.requireForeignRefused(t, keys[0], "k1")
	})

	t.Run("awaited refresh failed", func(t *testing.T) {
		h := newHarness(t)
		h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
		if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
			t.Fatalf("warm-up: %v", err)
		}
		h.jwks.set(serveFailure)
		h.clock.Advance(h.v.minRefreshInterval + time.Second)
		h.requireForeignRefused(t, keys[1], "k2")
		requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[1], "k2"))))
	})

	t.Run("known kid past grace with nothing to fall back on", func(t *testing.T) {
		h := newHarness(t)
		h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
		if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
			t.Fatalf("warm-up: %v", err)
		}
		h.clock.Advance(h.v.refreshInterval + h.v.refreshGrace + time.Second)
		// An attempt inside the minimum interval that neither failed nor
		// replaced the set: the unexpected state that fails closed.
		h.v.mu.Lock()
		h.v.lastAttempt, h.v.lastAttemptFailed = h.clock.Now(), false
		h.v.mu.Unlock()
		h.requireForeignRefused(t, keys[0], "k1")
		requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
	})
}

// Security review I-1: a foreign token never takes the refresh slot, neither
// as an unknown kid nor as a known kid whose set is past the refresh-ahead
// grace, and does not wait behind a stalled refresh.
func TestForeignTokenStartsNoRefresh(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.requireForeignRefused(t, keys[0], "k1")
	if got := h.jwks.fetches.Load(); got != 0 {
		t.Fatalf("cold cache fetched %d times for foreign tokens", got)
	}
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	h.jwks.set(serveStall)
	h.clock.Advance(h.v.refreshInterval + h.v.refreshGrace + time.Second)
	h.requireForeignRefused(t, keys[0], "k1")
	h.requireForeignRefused(t, keys[1], "k2")
	if got := h.jwks.fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want only the warm-up", got)
	}
}
