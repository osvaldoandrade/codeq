package jwks

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

// claimsAt are valid claims for issuer and audience, issued at iat.
func claimsAt(issuer string, audience any, iat time.Time) map[string]any {
	return map[string]any{claimIss: issuer, claimAud: audience, claimSub: testSubject, claimExp: iat.Unix() + 3600, claimIat: iat.Unix()}
}

// tokenFor signs a token for the unpublished kid "k2" with the harness clock
// as iat and the given issuer and audience claim.
func (h *testHarness) tokenFor(t *testing.T, issuer string, audience any) string {
	t.Helper()
	return signToken(t, testKeys(t)[1], "k2", claimsAt(issuer, audience, h.clock.Now()))
}

// Security review Low-1: an unknown kid with a fresh unverified iat earns 503
// only when the token names this validator's issuer and audience. A token for
// another issuer or audience could never be accepted, so it is refused (401)
// and cannot be used to force 503s; a matching one stays retryable.
func TestUnknownKidForeignIssuerOrAudienceIsRefused(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	h.clock.Advance(10 * time.Second)

	requireInvalid(t, mustErr(h.v.Validate(h.tokenFor(t, "https://other-issuer", testAudience))))
	requireInvalid(t, mustErr(h.v.Validate(h.tokenFor(t, testIssuer, "other-audience"))))
	requireInvalid(t, mustErr(h.v.Validate(h.tokenFor(t, testIssuer, []string{"a", "b"}))))
	requireInvalid(t, mustErr(h.v.Validate(h.tokenFor(t, "", testAudience))))

	requireUnavailable(t, mustErr(h.v.Validate(h.tokenFor(t, testIssuer, testAudience))))
	requireUnavailable(t, mustErr(h.v.Validate(h.tokenFor(t, testIssuer, []string{"other-audience", testAudience}))))
	if got := h.jwks.fetches.Load(); got != 1 {
		t.Fatalf("unknown-kid tokens drove %d fetches, want 1", got)
	}
}

// The same gate applies right after a waited refresh that did not publish
// the kid.
func TestUnknownKidAfterWaitedRefreshRequiresOwnIssuer(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	requireInvalid(t, mustErr(h.v.Validate(h.tokenFor(t, "https://other-issuer", testAudience))))
	h2 := newHarness(t)
	h2.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	requireUnavailable(t, mustErr(h2.v.Validate(h2.tokenFor(t, testIssuer, testAudience))))
}

func scopedValidator(t *testing.T, scope *auth.Scope, url, audience string, timeout time.Duration) *Validator {
	t.Helper()
	raw, err := json.Marshal(Config{JwksURL: url, Issuer: testIssuer, Audience: audience, HTTPTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewScopedValidator(scope, auth.ProviderConfig{Type: "jwks", Config: raw})
	if err != nil {
		t.Fatalf("NewScopedValidator: %v", err)
	}
	jv, ok := v.(*Validator)
	if !ok {
		t.Fatalf("NewScopedValidator built %T", v)
	}
	return jv
}

// Review P3-2: the producer and worker validators of one process share one
// key-set cache per JWKS URL (and HTTP timeout); each keeps its own issuer
// and audience. Another scope, URL or timeout gets its own cache.
func TestScopedValidatorsShareOneKeySetCachePerURL(t *testing.T) {
	keys := testKeys(t)
	fixture := newJWKSFixture(t)
	fixture.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	scope := auth.NewScope()
	producer := scopedValidator(t, scope, fixture.srv.URL, "producer-aud", time.Second)
	worker := scopedValidator(t, scope, fixture.srv.URL, "worker-aud", time.Second)
	if producer.keyCache != worker.keyCache {
		t.Fatal("validators of one scope and URL do not share their key-set cache")
	}
	if other := scopedValidator(t, auth.NewScope(), fixture.srv.URL, "producer-aud", time.Second); other.keyCache == producer.keyCache {
		t.Fatal("another scope shares the cache")
	}
	if slower := scopedValidator(t, scope, fixture.srv.URL, "producer-aud", 2*time.Second); slower.keyCache == producer.keyCache {
		t.Fatal("another HTTP timeout shares the cache")
	}
	if own, err := NewValidator(Config{JwksURL: fixture.srv.URL, Issuer: testIssuer, Audience: "producer-aud"}); err != nil || own.(*Validator).keyCache == producer.keyCache {
		t.Fatalf("an unscoped validator shares the cache (err=%v)", err)
	}

	sign := func(aud string) string {
		return signToken(t, keys[0], "k1", claimsAt(testIssuer, aud, time.Now().Add(-time.Minute)))
	}
	if _, err := producer.Validate(sign("producer-aud")); err != nil {
		t.Fatalf("producer token: %v", err)
	}
	if _, err := worker.Validate(sign("worker-aud")); err != nil {
		t.Fatalf("worker token: %v", err)
	}
	requireInvalid(t, mustErr(worker.Validate(sign("producer-aud"))))
	if got := fixture.fetches.Load(); got != 1 {
		t.Fatalf("two validators of one URL fetched %d times, want 1", got)
	}
}

// During a Tikti outage a request that tries the worker and then the producer
// validator waits for one refresh, not one per validator: the second sees the
// attempt that just failed and decides at once on the bounded stale set.
func TestSharedCacheWaitsForOneRefreshAcrossValidators(t *testing.T) {
	keys := testKeys(t)
	fixture := newJWKSFixture(t)
	fixture.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	scope := auth.NewScope()
	worker := scopedValidator(t, scope, fixture.srv.URL, "worker-aud", 200*time.Millisecond)
	producer := scopedValidator(t, scope, fixture.srv.URL, "producer-aud", 200*time.Millisecond)
	clock := &fakeClock{now: time.Now()}
	worker.now = clock.Now // the shared cache's clock
	sign := func(aud string) string {
		return signToken(t, keys[0], "k1", claimsAt(testIssuer, aud, clock.Now().Add(-time.Minute)))
	}
	if _, err := worker.Validate(sign("worker-aud")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	clock.Advance(worker.refreshInterval + worker.refreshGrace + time.Second)
	fixture.set(serveStall)

	ctx := context.Background()
	token := sign("producer-aud") // signed before any timing
	requireInvalid(t, mustErr(worker.ValidateContext(ctx, token)))
	started := time.Now()
	if _, err := producer.ValidateContext(ctx, token); err != nil {
		t.Fatalf("producer validator refused a known kid within the staleness bound: %v", err)
	}
	if waited := time.Since(started); waited >= worker.httpTimeout {
		t.Fatalf("the second validator waited %v for its own refresh", waited)
	}
	if got := fixture.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want the warm-up and one shared refresh", got)
	}
}
