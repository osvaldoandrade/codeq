package jwks

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Review F1: an idle validator whose set is older than the refresh-ahead
// grace must not accept a key Tikti already removed while Tikti answers.
func TestIdleValidatorRefusesRemovedKeyPastGrace(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	// Tikti drops k1 one minute after the fetch; the validator stays idle.
	h.clock.Advance(time.Minute)
	h.jwks.set(serveKeys, jwkFor("k2", &keys[1].PublicKey))
	h.clock.Advance(13*time.Minute + 50*time.Second)

	// The first request after the idle period waits for the refresh and is
	// decided on its result: no settle, no second request.
	requireInvalid(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
	if got := h.jwks.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2 (warm-up and the waited refresh)", got)
	}
	if _, err := h.v.Validate(h.token(t, keys[1], "k2")); err != nil {
		t.Fatalf("published key rejected: %v", err)
	}
}

// Every age between refreshInterval+refreshGrace and maxKeySetAge behaves the
// same: with Tikti healthy the removed key is refused at once.
func TestRemovedKeyRefusedAtEveryAgePastGrace(t *testing.T) {
	keys := testKeys(t)
	for _, age := range []time.Duration{
		defaultRefreshInterval + defaultRefreshGrace,
		defaultRefreshInterval + defaultRefreshGrace + time.Second,
		10 * time.Minute,
		defaultMaxKeySetAge,
	} {
		t.Run(age.String(), func(t *testing.T) {
			h := newHarness(t)
			h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
			if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
				t.Fatalf("warm-up: %v", err)
			}
			h.jwks.set(serveKeys, jwkFor("k2", &keys[1].PublicKey))
			h.clock.Advance(age)
			requireInvalid(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
		})
	}
}

// Inside the grace window the refresh stays in the background: a stalled
// Tikti does not delay the request, and the set is still used.
func TestWithinGraceRefreshDoesNotWait(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	h.jwks.set(serveStall)
	h.clock.Advance(h.v.refreshInterval + h.v.refreshGrace - time.Second)
	started := time.Now()
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("rejected inside the grace window: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("validation inside the grace window waited %v", elapsed)
	}
	h.settle(t)
}

// Past the grace window the stale set is used only after a refresh failed,
// within maxKeySetAge; beyond it the answer is 503, never an acceptance.
func TestPastGraceStaleSetOnlyAfterFailedRefresh(t *testing.T) {
	keys := testKeys(t)
	for name, mode := range map[string]serveMode{"failure": serveFailure, "stall": serveStall} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
			if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
				t.Fatalf("warm-up: %v", err)
			}
			h.jwks.set(mode)
			h.clock.Advance(h.v.refreshInterval + h.v.refreshGrace + time.Second)
			// Waits for the failing refresh (bounded by the HTTP timeout),
			// then falls back to the last good set.
			started := time.Now()
			if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
				t.Fatalf("outage: cached key rejected: %v", err)
			}
			if elapsed := time.Since(started); elapsed > h.v.httpTimeout+time.Second {
				t.Fatalf("wait %v exceeded the HTTP timeout %v", elapsed, h.v.httpTimeout)
			}
			// Within the minimum interval the recent failure is known: no
			// wait and no fetch.
			before := h.jwks.fetches.Load()
			started = time.Now()
			for i := 0; i < 10; i++ {
				if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
					t.Fatalf("outage: cached key rejected: %v", err)
				}
			}
			if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
				t.Fatalf("validation after a recent failure waited %v", elapsed)
			}
			if got := h.jwks.fetches.Load() - before; got != 0 {
				t.Fatalf("%d fetches inside the minimum interval", got)
			}
			// Up to the bound the set keeps verifying, then 503.
			h.clock.Advance(h.v.maxKeySetAge - h.v.refreshInterval - h.v.refreshGrace - 2*time.Second)
			if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
				t.Fatalf("rejected inside the bound: %v", err)
			}
			h.clock.Advance(2 * time.Second)
			requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
		})
	}
}

// Review F2: an unknown kid on a token issued after the set was fetched is
// 503 + Retry-After (the key may be newer than the set); a forged iat earns
// nothing beyond that status: no fetch, no acceptance, and it reverts to 401
// once a later refresh succeeds.
func TestUnknownKidIssuedAfterFetchIsRetryable(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	fetchedAt := h.clock.Now()
	h.clock.Advance(10 * time.Second)

	// Many forged kids with a fresh iat: 503 each, no fetch.
	for i := 0; i < 50; i++ {
		kid := "forged-" + big.NewInt(int64(i)).String()
		retry := requireUnavailable(t, mustErr(h.v.Validate(h.tokenIssuedAt(t, keys[1], kid, h.clock.Now()))))
		if retry < time.Second || retry > h.v.minRefreshInterval {
			t.Fatalf("Retry-After %v outside [1s, %v]", retry, h.v.minRefreshInterval)
		}
	}
	if got := h.jwks.fetches.Load(); got != 1 {
		t.Fatalf("forged iat drove %d fetches, want 1", got)
	}
	// iat before the fetch (beyond the skew) or in the future: refused.
	requireInvalid(t, mustErr(h.v.Validate(h.tokenIssuedAt(t, keys[1], "k2", fetchedAt.Add(-issuedAtSkew-time.Second)))))
	requireInvalid(t, mustErr(h.v.Validate(h.tokenIssuedAt(t, keys[1], "k2", h.clock.Now().Add(issuedAtSkew+time.Second)))))
	// Same second as the fetch: may be newer, retryable.
	requireUnavailable(t, mustErr(h.v.Validate(h.tokenIssuedAt(t, keys[1], "k2", fetchedAt))))

	// The client retries after Retry-After: the permitted refresh decides.
	issued := h.clock.Now()
	h.clock.Advance(h.v.minRefreshInterval)
	requireInvalid(t, mustErr(h.v.Validate(h.tokenIssuedAt(t, keys[1], "forged-0", issued))))
	if got := h.jwks.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2", got)
	}

	// A real rotation inside the interval: retryable, then accepted.
	h.jwks.set(serveKeys, jwkFor("k2", &keys[1].PublicKey))
	h.clock.Advance(time.Second)
	rotated := h.tokenIssuedAt(t, keys[1], "k2", h.clock.Now())
	requireUnavailable(t, mustErr(h.v.Validate(rotated)))
	h.clock.Advance(h.v.minRefreshInterval)
	if _, err := h.v.Validate(rotated); err != nil {
		t.Fatalf("rotated key rejected after the retry: %v", err)
	}
}

// A synchronous refresh that just succeeded also applies the iat rule: the
// token may postdate the fetch that answered it.
func TestUnknownKidAfterWaitedRefreshUsesIssuedAt(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	requireUnavailable(t, mustErr(h.v.Validate(h.tokenIssuedAt(t, keys[1], "k2", h.clock.Now()))))
	h2 := newHarness(t)
	h2.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	requireInvalid(t, mustErr(h2.v.Validate(h2.token(t, keys[1], "k2"))))
}

// Review F4: a waiter stops waiting when its own context ends; the shared
// refresh still completes for the others. Abandoning is never an acceptance.
func TestWaiterHonorsRequestContext(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	h.jwks.set(serveStall)
	h.clock.Advance(h.v.refreshInterval + h.v.refreshGrace)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	retry := requireUnavailable(t, mustErr(h.v.ValidateContext(ctx, h.token(t, keys[0], "k1"))))
	if elapsed := time.Since(started); elapsed >= h.v.httpTimeout {
		t.Fatalf("canceled waiter returned after %v, not before the HTTP timeout", elapsed)
	}
	if retry < time.Second {
		t.Fatalf("Retry-After %v below 1s", retry)
	}
	h.settle(t)
	if !strings.Contains(h.logs.String(), `"outcome":"fetch_timeout"`) {
		t.Fatalf("the shared refresh did not complete: %s", h.logs.String())
	}

	// An already-canceled context on a cold validator: 503, not a hang.
	cold := newHarness(t)
	cold.jwks.set(serveStall)
	done, stop := context.WithCancel(context.Background())
	stop()
	requireUnavailable(t, mustErr(cold.v.ValidateContext(done, cold.token(t, keys[0], "k1"))))
	cold.settle(t)
}

// Review F3: redirects stay on the configured origin.
func TestJWKSRedirectsStayOnTheConfiguredOrigin(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))

	same := http.NewServeMux()
	var sameURL string
	same.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sameURL+"/jwks", http.StatusFound)
	})
	same.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, h.jwks.srv.URL, http.StatusFound) // cross-origin
	})
	same.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sameURL+"/keys", http.StatusFound)
	})
	same.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		h.jwks.srv.Config.Handler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(same)
	t.Cleanup(srv.Close)
	sameURL = srv.URL

	validatorFor := func(path string) *Validator {
		v, err := newValidator(Config{JwksURL: srv.URL + path, Issuer: testIssuer, Audience: testAudience, HTTPTimeout: 250 * time.Millisecond})
		if err != nil {
			t.Fatalf("newValidator: %v", err)
		}
		v.now = h.clock.Now
		v.logger = h.v.logger
		return v
	}
	if _, err := validatorFor("/ok").Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("same-origin redirect not followed: %v", err)
	}
	// /moved → /jwks (same origin) → JWKS fixture (other port): refused.
	requireUnavailable(t, mustErr(validatorFor("/moved").Validate(h.token(t, keys[0], "k1"))))
	if !strings.Contains(h.logs.String(), `"outcome":"fetch_error"`) {
		t.Fatalf("cross-origin redirect not reported as fetch_error: %s", h.logs.String())
	}
}
