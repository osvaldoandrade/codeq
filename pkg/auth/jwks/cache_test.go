package jwks

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

const (
	testIssuer   = "test-issuer"
	testAudience = "test-audience"
	claimIss     = "iss"
	claimAud     = "aud"
	claimExp     = "exp"
	claimIat     = "iat"
	claimSub     = "sub"
	jwkKid       = "kid"
	jwkKty       = "kty"
	jwksKeys     = "keys"
)

var (
	keyOnce   sync.Once
	sharedKey [3]*rsa.PrivateKey
)

// testKeys returns three 2048-bit keys generated once per test binary.
func testKeys(t *testing.T) [3]*rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		for i := range sharedKey {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			sharedKey[i] = k
		}
	})
	return sharedKey
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type serveMode int

const (
	serveKeys serveMode = iota
	serveFailure
	serveStall
	serveRaw
)

// jwksFixture is a JWKS endpoint whose content and failure mode tests change.
type jwksFixture struct {
	srv     *httptest.Server
	fetches atomic.Int64
	stall   chan struct{}

	mu   sync.Mutex
	mode serveMode
	keys []map[string]any
	raw  []byte
}

func newJWKSFixture(t *testing.T) *jwksFixture {
	t.Helper()
	f := &jwksFixture{stall: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.fetches.Add(1)
		f.mu.Lock()
		mode, keys, raw := f.mode, f.keys, f.raw
		f.mu.Unlock()
		switch mode {
		case serveKeys:
			_ = json.NewEncoder(w).Encode(map[string]any{jwksKeys: keys})
		case serveFailure:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case serveStall:
			select {
			case <-f.stall:
			case <-r.Context().Done():
			}
		case serveRaw:
			_, _ = w.Write(raw)
		}
	}))
	t.Cleanup(func() {
		close(f.stall)
		f.srv.Close()
	})
	return f
}

func (f *jwksFixture) set(mode serveMode, keys ...map[string]any) {
	if keys == nil {
		keys = []map[string]any{} // {"keys":[]}, never null
	}
	f.mu.Lock()
	f.mode, f.keys = mode, keys
	f.mu.Unlock()
}

func (f *jwksFixture) setRaw(raw []byte) {
	f.mu.Lock()
	f.mode, f.raw = serveRaw, raw
	f.mu.Unlock()
}

func jwkFor(kid string, pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		jwkKty: ktyRSA, jwkKid: kid, "use": useSignature,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

type testHarness struct {
	v     *Validator
	clock *fakeClock
	jwks  *jwksFixture
	logs  *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	fixture := newJWKSFixture(t)
	v, err := newValidator(Config{
		JwksURL: fixture.srv.URL, Issuer: testIssuer, Audience: testAudience,
		ClockSkew: time.Second, HTTPTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newValidator: %v", err)
	}
	clock := &fakeClock{now: time.Now()}
	logs := &syncBuffer{}
	v.now = clock.Now
	v.logger = slog.New(slog.NewJSONHandler(logs, nil))
	return &testHarness{v: v, clock: clock, jwks: fixture, logs: logs}
}

// token signs a valid token for kid, issued an hour before the fake clock
// (before any key-set fetch of a test), whose exp follows the fake clock.
func (h *testHarness) token(t *testing.T, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	return h.tokenIssuedAt(t, key, kid, h.clock.Now().Add(-time.Hour))
}

// tokenIssuedAt signs a valid token for kid with the given iat.
func (h *testHarness) tokenIssuedAt(t *testing.T, key *rsa.PrivateKey, kid string, iat time.Time) string {
	t.Helper()
	now := h.clock.Now().Unix()
	return signToken(t, key, kid, map[string]any{
		claimIss: testIssuer, claimAud: testAudience, claimSub: "svc", claimExp: now + 3600, claimIat: iat.Unix(),
	})
}

// settle waits until no refresh is in flight.
func (h *testHarness) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.v.mu.Lock()
		idle := h.v.inflight == nil
		h.v.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("refresh did not finish")
}

func requireInvalid(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("expected invalid token (401), got %v", err)
	}
	if _, unavailable := auth.RetryAfter(err); unavailable {
		t.Fatalf("refusal must not be classified unavailable: %v", err)
	}
}

func requireUnavailable(t *testing.T, err error) time.Duration {
	t.Helper()
	retry, ok := auth.RetryAfter(err)
	if !ok {
		t.Fatalf("expected unavailable (503), got %v", err)
	}
	if errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("unavailability must not be classified as refusal: %v", err)
	}
	return retry
}

func TestConcurrentUnknownKidsFetchAtMostOncePerInterval(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))

	const parallel = 64
	attack := func(round int) {
		tokens := make([]string, parallel)
		for i := range tokens {
			tokens[i] = h.token(t, keys[1], "attacker-"+big.NewInt(int64(round*parallel+i)).String())
		}
		var wg sync.WaitGroup
		for _, token := range tokens {
			wg.Add(1)
			go func(token string) {
				defer wg.Done()
				_, err := h.v.Validate(token)
				if !errors.Is(err, auth.ErrInvalidToken) {
					t.Errorf("unknown kid after a successful refresh: want 401, got %v", err)
				}
			}(token)
		}
		wg.Wait()
		h.settle(t)
	}

	attack(0)
	if got := h.jwks.fetches.Load(); got != 1 {
		t.Fatalf("cold start under %d parallel unknown kids fetched %d times, want 1", parallel, got)
	}
	h.clock.Advance(h.v.minRefreshInterval / 2)
	attack(1)
	if got := h.jwks.fetches.Load(); got != 1 {
		t.Fatalf("unknown kids within the interval fetched %d times, want 1", got)
	}
	// The legitimate key keeps working without any fetch.
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	h.clock.Advance(h.v.minRefreshInterval)
	attack(2)
	if got := h.jwks.fetches.Load(); got != 2 {
		t.Fatalf("after one interval fetched %d times in total, want 2", got)
	}
}

func TestStallKeepsLastGoodSetWithinBoundThen503(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	h.jwks.set(serveStall)
	// Past the refresh interval: the token is accepted from the cached set
	// without waiting for the stalled identity provider.
	h.clock.Advance(h.v.refreshInterval + time.Second)
	started := time.Now()
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("cached valid token rejected during stall: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("validation waited %v for a background refresh", elapsed)
	}
	h.settle(t)

	// Failing refreshes are spaced by the minimum interval.
	before := h.jwks.fetches.Load()
	for i := 0; i < 20; i++ {
		if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
			t.Fatalf("cached valid token rejected: %v", err)
		}
	}
	h.settle(t)
	if got := h.jwks.fetches.Load() - before; got != 0 {
		t.Fatalf("stalled provider received %d extra fetches within the interval", got)
	}

	// An unknown kid while the last refresh failed cannot be decided: 503.
	requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[1], "k-new"))))

	// Just inside the bound the cached key still verifies.
	h.clock.Advance(h.v.maxKeySetAge - h.v.refreshInterval - 2*time.Second)
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("token rejected inside the bound: %v", err)
	}
	h.settle(t)

	// Beyond the bound: no usable key set, 503 with Retry-After, never 401.
	h.clock.Advance(2 * time.Second)
	retry := requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
	if retry < time.Second || retry > h.v.minRefreshInterval {
		t.Fatalf("Retry-After %v outside [1s, %v]", retry, h.v.minRefreshInterval)
	}
	// A failed synchronous attempt beyond the bound is still 503.
	h.jwks.set(serveFailure)
	h.clock.Advance(h.v.minRefreshInterval)
	requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))

	// Recovery: the next successful refresh restores acceptance.
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	h.clock.Advance(h.v.minRefreshInterval)
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("token rejected after recovery: %v", err)
	}

	logs := h.logs.String()
	if !strings.Contains(logs, `"outcome":"fetch_timeout"`) || !strings.Contains(logs, `"outcome":"http_status"`) {
		t.Fatalf("refresh failures not logged with fixed outcomes: %s", logs)
	}
	if strings.Contains(logs, h.jwks.srv.URL) || strings.Contains(logs, "eyJ") {
		t.Fatalf("logs leak the JWKS URL or token data: %s", logs)
	}
}

func TestColdStartFailureIs503WithoutLeakingDetails(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveFailure)
	err := mustErr(h.v.Validate(h.token(t, keys[0], "k1")))
	requireUnavailable(t, err)
	if strings.Contains(err.Error(), h.jwks.srv.URL) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error leaks the JWKS location: %q", err.Error())
	}
	// Further requests inside the interval do not reach the provider.
	for i := 0; i < 10; i++ {
		requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
	}
	if got := h.jwks.fetches.Load(); got != 1 {
		t.Fatalf("cold-start failures fetched %d times, want 1", got)
	}
}

func TestRotationAcceptsNewKidAndDropsRemovedKid(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("k1 before rotation: %v", err)
	}

	h.jwks.set(serveKeys, jwkFor("k2", &keys[1].PublicKey))
	// Inside the interval the new kid is negatively cached. A token issued
	// before the set was fetched is refused; one issued after it may carry a
	// key published since (Tikti does not pre-publish), so it is answered
	// 503 + Retry-After until the next permitted refresh, never accepted.
	requireInvalid(t, mustErr(h.v.Validate(h.token(t, keys[1], "k2"))))
	h.clock.Advance(time.Second)
	retry := requireUnavailable(t, mustErr(h.v.Validate(h.tokenIssuedAt(t, keys[1], "k2", h.clock.Now()))))
	if retry < time.Second || retry > h.v.minRefreshInterval {
		t.Fatalf("Retry-After %v outside [1s, %v]", retry, h.v.minRefreshInterval)
	}
	if got := h.jwks.fetches.Load(); got != 1 {
		t.Fatalf("unknown kids inside the interval fetched %d times, want 1", got)
	}
	h.clock.Advance(h.v.minRefreshInterval)
	if _, err := h.v.Validate(h.token(t, keys[1], "k2")); err != nil {
		t.Fatalf("rotated key rejected after refresh: %v", err)
	}
	// k1 was removed by a successful refresh: refused at once, no fetch.
	before := h.jwks.fetches.Load()
	requireInvalid(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
	if h.jwks.fetches.Load() != before {
		t.Fatal("removed kid triggered a fetch inside the interval")
	}
}

func TestRefreshAheadSwapsTheWholeSet(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey), jwkFor("k2", &keys[1].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	h.jwks.set(serveKeys, jwkFor("k2", &keys[1].PublicKey))
	h.clock.Advance(h.v.refreshInterval)
	// Served from the old set while the background refresh runs.
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("k1 rejected before the refresh completed: %v", err)
	}
	h.settle(t)
	requireInvalid(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
	if _, err := h.v.Validate(h.token(t, keys[1], "k2")); err != nil {
		t.Fatalf("k2 rejected: %v", err)
	}
}

func TestOversizeJWKSBodyIsRejected(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.setRaw(bytes.Repeat([]byte(" "), maxJWKSBytes+1))
	requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
	if !strings.Contains(h.logs.String(), `"outcome":"too_large"`) {
		t.Fatalf("oversize body not reported: %s", h.logs.String())
	}

	// With a good set cached, an oversize refresh keeps the good set.
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	h.clock.Advance(h.v.minRefreshInterval)
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	h.jwks.setRaw(bytes.Repeat([]byte(" "), maxJWKSBytes+1))
	h.clock.Advance(h.v.refreshInterval)
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("good set dropped by an oversize refresh: %v", err)
	}
	h.settle(t)
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("good set dropped by an oversize refresh: %v", err)
	}
}

func TestSmallRSAKeyIsNeverUsable(t *testing.T) {
	keys := testKeys(t)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate 1024-bit key: %v", err)
	}
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("small", &small.PublicKey), jwkFor("k1", &keys[0].PublicKey))
	requireInvalid(t, mustErr(h.v.Validate(h.token(t, small, "small"))))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("2048-bit key beside a small key rejected: %v", err)
	}

	// A set holding only small keys verifies nothing.
	only := newHarness(t)
	only.jwks.set(serveKeys, jwkFor("small", &small.PublicKey))
	requireInvalid(t, mustErr(only.v.Validate(only.token(t, small, "small"))))
	if !strings.Contains(only.logs.String(), `"outcome":"no_usable_keys"`) {
		t.Fatalf("small-key set not reported: %s", only.logs.String())
	}
}

func TestAuthoritativeEmptySetRevokesEveryKey(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	h.jwks.set(serveKeys) // {"keys":[]}
	h.clock.Advance(h.v.refreshInterval)
	_, _ = h.v.Validate(h.token(t, keys[0], "k1")) // starts the refresh-ahead
	h.settle(t)
	requireInvalid(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
}

func TestExpHasZeroLeewayDespiteClockSkew(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	v, err := newValidator(Config{JwksURL: h.jwks.srv.URL, Issuer: testIssuer, Audience: testAudience, ClockSkew: time.Hour})
	if err != nil {
		t.Fatalf("newValidator: %v", err)
	}
	now := time.Now().Unix()
	token := signToken(t, keys[0], "k1", map[string]any{claimIss: testIssuer, claimAud: testAudience, claimExp: now - 2})
	requireInvalid(t, mustErr(v.Validate(token)))
}

func TestMissingExpIsRefused(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	token := signToken(t, keys[0], "k1", map[string]any{claimIss: testIssuer, claimAud: testAudience, claimSub: "svc"})
	err := mustErr(h.v.Validate(token))
	requireInvalid(t, err)
	if !strings.Contains(err.Error(), "required claim missing") {
		t.Fatalf("unexpected reason: %v", err)
	}
}

func TestBadSignatureWithFreshKeysIsRefused(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
	// Signed by another key but claiming kid k1.
	requireInvalid(t, mustErr(h.v.Validate(h.token(t, keys[1], "k1"))))
}

func TestDisallowedAlgorithmsNeverFetch(t *testing.T) {
	h := newHarness(t)
	h.jwks.set(serveKeys)
	requireInvalid(t, mustErr(h.v.Validate(hs256Token(t))))
	none := strings.Join([]string{
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"test-issuer"}`)), "",
	}, ".")
	requireInvalid(t, mustErr(h.v.Validate(none)))
	requireInvalid(t, mustErr(h.v.Validate(strings.Repeat("a", maxTokenBytes+1))))
	if got := h.jwks.fetches.Load(); got != 0 {
		t.Fatalf("disallowed tokens caused %d fetches", got)
	}
}

func TestJWKAlgMustMatchToken(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	jwk := jwkFor("k1", &keys[0].PublicKey)
	jwk["alg"] = "RS512"
	h.jwks.set(serveKeys, jwk)
	requireInvalid(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1")))) // RS256 token
}

func TestUnreadableDocumentsKeepTheLastGoodSet(t *testing.T) {
	keys := testKeys(t)
	for name, body := range map[string]string{
		"missing keys": `{}`,
		"null keys":    `{"keys":null}`,
		"html":         `<html>ok</html>`,
		"trailing":     `{"keys":[]} {"keys":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey))
			if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
				t.Fatalf("warm-up: %v", err)
			}
			h.jwks.setRaw([]byte(body))
			h.clock.Advance(h.v.refreshInterval)
			_, _ = h.v.Validate(h.token(t, keys[0], "k1"))
			h.settle(t)
			if _, err := h.v.Validate(h.token(t, keys[0], "k1")); err != nil {
				t.Fatalf("good set dropped by an unreadable document: %v", err)
			}
			if !strings.Contains(h.logs.String(), `"outcome":"invalid_document"`) {
				t.Fatalf("not reported as invalid_document: %s", h.logs.String())
			}
		})
	}
}

func TestDuplicateKidFailsTheRefresh(t *testing.T) {
	keys := testKeys(t)
	h := newHarness(t)
	h.jwks.set(serveKeys, jwkFor("k1", &keys[0].PublicKey), jwkFor("k1", &keys[1].PublicKey))
	requireUnavailable(t, mustErr(h.v.Validate(h.token(t, keys[0], "k1"))))
	if !strings.Contains(h.logs.String(), `"outcome":"duplicate_kid"`) {
		t.Fatalf("duplicate kid not reported: %s", h.logs.String())
	}
}

func TestParseRSAPublicKeyBounds(t *testing.T) {
	keys := testKeys(t)
	good := jwkFor("k", &keys[0].PublicKey)
	if _, err := parseRSAPublicKey(good["n"].(string), good["e"].(string)); err != nil {
		t.Fatalf("2048-bit key rejected: %v", err)
	}
	n := good["n"].(string)
	for name, e := range map[string]string{
		"even":  base64.RawURLEncoding.EncodeToString([]byte{0x02}),
		"one":   base64.RawURLEncoding.EncodeToString([]byte{0x01}),
		"long":  base64.RawURLEncoding.EncodeToString([]byte{1, 0, 0, 0, 1}),
		"empty": "",
	} {
		if _, err := parseRSAPublicKey(n, e); err == nil {
			t.Fatalf("exponent %s accepted", name)
		}
	}
	huge := base64.RawURLEncoding.EncodeToString(append([]byte{0x80}, make([]byte, maxRSAKeyBits/8)...))
	if _, err := parseRSAPublicKey(huge, good["e"].(string)); err == nil {
		t.Fatal("modulus above the maximum accepted")
	}
}

func TestRefuseUnsafeRedirect(t *testing.T) {
	req := func(raw string) *http.Request { return &http.Request{URL: mustURL(t, raw)} }
	origin := req("https://idp.example/jwks")
	for _, target := range []string{"https://idp.example/keys", "https://IDP.example/jwks?v=2"} {
		if err := refuseUnsafeRedirect(req(target), []*http.Request{origin}); err != nil {
			t.Fatalf("same-origin redirect to %s refused: %v", target, err)
		}
	}
	for _, target := range []string{
		"http://idp.example/jwks",        // scheme downgrade
		"https://idp.example:8443/jwks",  // other port
		"https://evil.example/jwks",      // other host
		"https://idp.example.evil/jwks",  // suffix confusion
		"https://user@evil.example/jwks", // userinfo does not change the host check
	} {
		if err := refuseUnsafeRedirect(req(target), []*http.Request{origin}); err == nil {
			t.Fatalf("cross-origin redirect to %s allowed", target)
		}
	}
	plain := req("http://idp.example/jwks")
	if err := refuseUnsafeRedirect(req("https://idp.example/jwks"), []*http.Request{plain}); err == nil {
		t.Fatal("scheme change allowed (the origin is scheme, host and port)")
	}
	if err := refuseUnsafeRedirect(req("https://idp.example/4"), []*http.Request{origin, origin, origin}); err == nil {
		t.Fatal("redirect chain not bounded")
	}
}

func TestNewValidatorBoundsTimeout(t *testing.T) {
	v, err := newValidator(Config{JwksURL: "http://x", Issuer: "i", Audience: "a", HTTPTimeout: time.Hour})
	if err != nil || v.httpTimeout != maxHTTPTimeout {
		t.Fatalf("timeout not capped: %v %v", v, err)
	}
}

func mustErr(_ *auth.Claims, err error) error { return err }

// hs256Token builds an HMAC-signed token naming kid k1: the classic
// algorithm-confusion attempt against an RSA verifier.
func hs256Token(t *testing.T) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]any{"alg": "HS256", jwkKid: "k1"}) + "." +
		enc(map[string]any{claimIss: testIssuer, claimAud: testAudience, claimExp: time.Now().Add(time.Hour).Unix()})
	mac := hmac.New(sha256.New, []byte("shared-secret-for-test"))
	mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
