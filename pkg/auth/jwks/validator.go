// Package jwks validates RSA-signed JWTs against a remote JSON Web Key Set.
//
// The key set is cached as a whole and refreshed by at most one request at a
// time, never more often than minRefreshInterval, so unauthenticated callers
// cannot amplify traffic to the identity provider. A set older than
// refreshInterval is refreshed in the background; one older than
// refreshInterval+refreshGrace is not trusted again until a refresh has been
// attempted, so a key the provider removed stops verifying within that age
// whenever the provider answers. Only when refreshes fail does the last
// successfully fetched set keep verifying tokens, up to maxKeySetAge; beyond
// that, or before the first successful fetch, validation reports
// auth.ErrUnavailable (HTTP 503) instead of accepting or refusing. ADR 0014.
package jwks

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

const (
	// defaultRefreshInterval is the key-set age after which the next
	// validation starts a background refresh (it does not wait for it).
	defaultRefreshInterval = 5 * time.Minute
	// defaultRefreshGrace extends the non-blocking refresh-ahead window. A
	// set older than refreshInterval+refreshGrace is used again only after
	// the validation waited for a refresh that failed (or a recent attempt
	// failed), so an idle validator never trusts a set Tikti could have
	// replaced. See ADR 0014.
	defaultRefreshGrace = time.Minute
	// defaultMinRefreshInterval spaces refresh attempts of any cause:
	// unknown kid, refresh-ahead, retry after a failure, or cold start.
	defaultMinRefreshInterval = 30 * time.Second
	// defaultMaxKeySetAge bounds how long the last successfully fetched key
	// set keeps verifying tokens while refreshes fail. See ADR 0014.
	defaultMaxKeySetAge = 15 * time.Minute

	defaultHTTPTimeout = 5 * time.Second
	maxHTTPTimeout     = 10 * time.Second
	maxRedirects       = 3

	// issuedAtSkew is the clock tolerance applied when an unknown kid's
	// unverified iat is compared with the key set's fetch time (F2 of the
	// ADR 0014 review): such a token may be signed by a key published after
	// the fetch and is answered 503 + Retry-After instead of 401.
	issuedAtSkew = 5 * time.Second

	// maxJWKSBytes bounds a JWKS response body (Tikti publishes ~0.5 KiB).
	maxJWKSBytes = 1 << 20
	// maxTokenBytes bounds the compact JWT accepted for parsing.
	maxTokenBytes = 64 << 10
	minRSAKeyBits = 2048
	maxRSAKeyBits = 8192

	ktyRSA       = "RSA"
	useSignature = "sig"
)

// allowedAlgorithms is the RSA PKCS#1 v1.5 family the validator has always
// accepted. none, HMAC, ECDSA, EdDSA and RSA-PSS are refused.
var allowedAlgorithms = []string{"RS256", "RS384", "RS512"}

// Config contains JWKS validator configuration.
//
// ClockSkew is accepted for configuration compatibility but does not extend
// exp or nbf: they are enforced with zero leeway, as before, so an issued
// token is usable for at most its own lifetime (ADR 0003, platform ADR-0022
// C1.4 revocation bound).
type Config struct {
	JwksURL     string        `json:"jwksUrl"`
	Issuer      string        `json:"issuer"`
	Audience    string        `json:"audience"`
	ClockSkew   time.Duration `json:"clockSkew"`
	HTTPTimeout time.Duration `json:"httpTimeout"`
}

// verificationKey is one usable signing key of a fetched set.
type verificationKey struct {
	key *rsa.PublicKey
	alg string // optional JWK "alg"; empty accepts any allowed algorithm
}

// keySet is an immutable, successfully fetched and validated JWKS.
type keySet struct {
	keys      map[string]verificationKey
	fetchedAt time.Time
}

// refreshCall is the single in-flight refresh shared by all waiters.
type refreshCall struct {
	done chan struct{}
	err  error
}

// Validator validates JWT tokens using JWKS. Its key-set cache may be shared
// with the other validators of the same auth.Scope and key-set URL.
type Validator struct {
	*keyCache
	issuer   string
	audience string
	parser   *jwt.Parser
}

// keyCache is the key set of one JWKS URL, its refresh state and its
// bounds. Validators built in one auth.Scope for the same URL and HTTP
// timeout share it, so the producer and worker validators of a process
// fetch, rate-limit and wait together (ADR 0014). It holds no issuer or
// audience: every validator applies its own.
type keyCache struct {
	jwksURL string
	client  *http.Client
	logger  *slog.Logger
	now     func() time.Time

	httpTimeout        time.Duration
	refreshInterval    time.Duration
	refreshGrace       time.Duration
	minRefreshInterval time.Duration
	maxKeySetAge       time.Duration

	mu sync.Mutex
	// set is the last successfully fetched key set; nil until the first.
	set *keySet
	// lastAttempt is when the most recent refresh attempt started.
	lastAttempt time.Time
	// lastAttemptFailed is true while the most recent finished attempt failed.
	lastAttemptFailed bool
	inflight          *refreshCall
}

func init() {
	// Register JWKS provider in the global registry
	auth.RegisterProvider("jwks", NewValidatorFromJSON)
	auth.RegisterScopedProvider("jwks", newScopedValidatorFromJSON)
}

// NewValidatorFromJSON creates a validator from JSON configuration
func NewValidatorFromJSON(configJSON json.RawMessage) (auth.Validator, error) {
	var cfg Config
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("invalid JWKS config: %w", err)
	}
	return NewValidator(cfg)
}

// NewValidator creates a new JWKS validator with its own key-set cache.
func NewValidator(cfg Config) (auth.Validator, error) {
	return newValidator(cfg)
}

// newScopedValidatorFromJSON is NewValidatorFromJSON sharing the key-set
// cache of scope's other JWKS validators for the same URL and HTTP timeout.
func newScopedValidatorFromJSON(configJSON json.RawMessage, scope *auth.Scope) (auth.Validator, error) {
	var cfg Config
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("invalid JWKS config: %w", err)
	}
	return newSharedValidator(cfg, scope)
}

// cacheKey identifies a shared key-set cache within an auth.Scope.
type cacheKey struct {
	jwksURL     string
	httpTimeout time.Duration
}

func newValidator(cfg Config) (*Validator, error) {
	return newSharedValidator(cfg, nil)
}

// newSharedValidator builds a validator whose key-set cache is shared through
// scope; a nil scope gives it its own.
func newSharedValidator(cfg Config, scope *auth.Scope) (*Validator, error) {
	if cfg.JwksURL == "" {
		return nil, errors.New("jwksURL is required")
	}
	if cfg.Issuer == "" {
		return nil, errors.New("issuer is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("audience is required")
	}
	timeout := cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	timeout = min(timeout, maxHTTPTimeout)

	var cache *keyCache
	if scope != nil {
		cache, _ = scope.Shared(cacheKey{jwksURL: cfg.JwksURL, httpTimeout: timeout}, func() any {
			return newKeyCache(cfg.JwksURL, timeout)
		}).(*keyCache)
	}
	if cache == nil {
		cache = newKeyCache(cfg.JwksURL, timeout)
	}
	v := &Validator{keyCache: cache, issuer: cfg.Issuer, audience: cfg.Audience}
	v.parser = jwt.NewParser(
		jwt.WithValidMethods(allowedAlgorithms),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return v.now() }),
	)
	return v, nil
}

func newKeyCache(jwksURL string, timeout time.Duration) *keyCache {
	return &keyCache{
		jwksURL:            jwksURL,
		httpTimeout:        timeout,
		refreshInterval:    defaultRefreshInterval,
		refreshGrace:       defaultRefreshGrace,
		minRefreshInterval: defaultMinRefreshInterval,
		maxKeySetAge:       defaultMaxKeySetAge,
		logger:             slog.Default(),
		now:                time.Now,
		client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: refuseUnsafeRedirect,
		},
	}
}

// refuseUnsafeRedirect bounds redirects and keeps them on the configured
// origin (scheme, host and port): the key set is trusted because of where it
// was fetched from, so a redirect to another origin, a downgrade included,
// fails the refresh.
func refuseUnsafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many JWKS redirects")
	}
	if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
		return errors.New("JWKS redirect leaves the configured origin")
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// Validate validates a JWT token without a request context; see
// ValidateContext.
func (v *Validator) Validate(tokenString string) (*auth.Claims, error) {
	return v.ValidateContext(context.Background(), tokenString)
}

// ValidateContext validates a JWT token. Refusals wrap auth.ErrInvalidToken; a
// token that cannot be evaluated returns *auth.UnavailableError. A wait for a
// key-set refresh ends at the HTTP timeout or when ctx ends, whichever is
// first; an abandoned wait is unavailable, never accepted. Error messages
// never carry token content, the JWKS URL or transport details.
func (v *Validator) ValidateContext(ctx context.Context, tokenString string) (*auth.Claims, error) {
	if tokenString == "" || len(tokenString) > maxTokenBytes {
		return nil, invalid("malformed token")
	}
	var keyErr error
	claims := jwt.MapClaims{}
	token, err := v.parser.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			keyErr = invalid("missing kid")
			return nil, keyErr
		}
		key, err := v.keyFor(ctx, kid, token.Method.Alg(), v.unverifiedHint(claims))
		if err != nil {
			keyErr = err
			return nil, err
		}
		return key, nil
	})
	if keyErr != nil {
		// The key lookup decided first (unknown kid, unavailable set, alg
		// mismatch); its classification wins over the parser's wrapping.
		return nil, keyErr
	}
	if err != nil {
		return nil, invalid(parseFailureReason(err))
	}
	if token == nil || !token.Valid {
		return nil, invalid("invalid token")
	}
	return buildClaims(claims), nil
}

func parseFailureReason(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return "token expired"
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return "required claim missing"
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return "invalid issuer"
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return "invalid audience"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "invalid signature"
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return "token not valid yet"
	default:
		return "invalid token"
	}
}

// invalid wraps auth.ErrInvalidToken with a fixed, token-free reason.
func invalid(reason string) error {
	return fmt.Errorf("%w: %s", auth.ErrInvalidToken, reason)
}

func buildClaims(claims jwt.MapClaims) *auth.Claims {
	iss, _ := claims["iss"].(string)
	audiences, _ := claims.GetAudience()
	result := &auth.Claims{
		Subject:  getStringClaim(claims, "sub"),
		Email:    getStringClaim(claims, "email"),
		Issuer:   iss,
		Audience: []string(audiences),
		Raw:      claims,
	}
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		result.ExpiresAt = exp.Time
	}
	if iat, err := claims.GetIssuedAt(); err == nil && iat != nil {
		result.IssuedAt = iat.Time
	}
	if scope, ok := claims["scope"].(string); ok {
		result.Scopes = strings.Fields(scope)
	}
	if eventTypes, ok := claims["eventTypes"].([]interface{}); ok {
		for _, et := range eventTypes {
			if etStr, ok := et.(string); ok {
				result.EventTypes = append(result.EventTypes, etStr)
			}
		}
	}
	return result
}

// tokenHint is what an unknown kid's 503-or-401 choice reads from the token
// before its signature is checked. It only ever selects 503 over 401; it
// never contributes to an acceptance.
type tokenHint struct {
	// issuedAt is the unverified iat; zero when absent or malformed.
	issuedAt time.Time
	// ours is set when the unverified iss equals this validator's issuer and
	// the unverified aud contains its audience: only such a token could be
	// accepted once the key is known.
	ours bool
}

func (v *Validator) unverifiedHint(claims jwt.MapClaims) tokenHint {
	var hint tokenHint
	if iat, err := claims.GetIssuedAt(); err == nil && iat != nil {
		hint.issuedAt = iat.Time
	}
	iss, err := claims.GetIssuer()
	if err != nil || iss != v.issuer {
		return hint
	}
	audiences, err := claims.GetAudience()
	if err != nil {
		return hint
	}
	for _, aud := range audiences {
		if aud == v.audience {
			hint.ours = true
			break
		}
	}
	return hint
}

// keyFor returns the verification key for kid from a usable key set.
//
//   - Known kid, set younger than refreshInterval: returned at once.
//   - Known kid, set younger than refreshInterval+refreshGrace: returned at
//     once and one background refresh starts.
//   - Known kid, older set: waits for a refresh (joined or started) and
//     decides on its result; the old set is used only when that refresh, or
//     the attempt made within minRefreshInterval, failed, up to maxKeySetAge.
//   - Unknown kid: joins the in-flight refresh or starts one when
//     minRefreshInterval has elapsed since the last attempt; otherwise the
//     kid is negatively cached for the rest of that interval.
//   - No usable set: *auth.UnavailableError.
func (c *keyCache) keyFor(ctx context.Context, kid, alg string, hint tokenHint) (*rsa.PublicKey, error) {
	c.mu.Lock()
	now := c.now()
	set := c.usableSetLocked(now)
	if key, ok := set.lookup(kid); ok {
		return c.knownKidLocked(ctx, set, key, now, kid, alg, hint)
	}
	return c.unknownKidLocked(ctx, set, now, kid, alg, hint)
}

// knownKidLocked decides a kid present in set. It is called with c.mu held
// and releases it.
func (c *keyCache) knownKidLocked(ctx context.Context, set *keySet, key verificationKey, now time.Time, kid, alg string, hint tokenHint) (*rsa.PublicKey, error) {
	age := now.Sub(set.fetchedAt)
	if age < c.refreshInterval+c.refreshGrace {
		if age >= c.refreshInterval {
			c.startRefreshLocked(now, triggerRefreshAhead)
		}
		c.mu.Unlock()
		observeDecision(stateFor(age, c.refreshInterval), age)
		return checkAlg(key, alg)
	}
	call := c.startRefreshLocked(now, triggerRefreshAhead)
	if call != nil {
		c.mu.Unlock()
		return c.awaitKey(ctx, call, kid, alg, hint)
	}
	// The attempt made within minRefreshInterval did not replace this set,
	// so it failed: Tikti is unreachable and the set stays usable up to
	// maxKeySetAge. Anything else is unexpected and fails closed.
	failed := c.lastAttemptFailed
	retry := c.retryAfterLocked(now)
	c.mu.Unlock()
	if !failed {
		observeDecision(decisionUnavailable, 0)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}
	observeDecision(decisionStale, age)
	return checkAlg(key, alg)
}

// unknownKidLocked decides a kid absent from set (which may be nil). It is
// called with c.mu held and releases it.
func (c *keyCache) unknownKidLocked(ctx context.Context, set *keySet, now time.Time, kid, alg string, hint tokenHint) (*rsa.PublicKey, error) {
	call := c.startRefreshLocked(now, triggerUnknownKid)
	if call != nil {
		c.mu.Unlock()
		return c.awaitKey(ctx, call, kid, alg, hint)
	}
	lastFailed := c.lastAttemptFailed
	retry := c.retryAfterLocked(now)
	c.mu.Unlock()
	if set != nil && !lastFailed {
		// Absent from a set fetched successfully within the interval.
		return nil, c.unknownKid(set, now, hint, retry)
	}
	observeDecision(decisionUnavailable, 0)
	return nil, &auth.UnavailableError{RetryAfter: retry}
}

// awaitKey waits for call (bounded by the HTTP timeout and ctx) and decides
// kid on the key set it left behind.
func (c *keyCache) awaitKey(ctx context.Context, call *refreshCall, kid, alg string, hint tokenHint) (*rsa.PublicKey, error) {
	select {
	case <-call.done:
	case <-ctx.Done():
		// The shared refresh continues for the other waiters; this caller
		// is gone and gets no decision, which is never an acceptance.
		c.mu.Lock()
		retry := c.retryAfterLocked(c.now())
		c.mu.Unlock()
		observeDecision(decisionCanceled, 0)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}

	c.mu.Lock()
	now := c.now()
	set := c.usableSetLocked(now)
	retry := c.retryAfterLocked(now)
	c.mu.Unlock()
	if set == nil {
		observeDecision(decisionUnavailable, 0)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}
	age := now.Sub(set.fetchedAt)
	if key, ok := set.keys[kid]; ok {
		// Either the refresh succeeded and still publishes kid, or it failed
		// and the previous set is within maxKeySetAge.
		observeDecision(stateFor(age, c.refreshInterval), age)
		return checkAlg(key, alg)
	}
	if call.err != nil {
		// The refresh that could have published this kid failed: the token
		// can be neither verified nor refused yet.
		observeDecision(decisionUnavailable, age)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}
	return nil, c.unknownKid(set, now, hint, retry)
}

// unknownKid classifies a kid absent from a successfully fetched set. A token
// that names this validator's issuer and audience and whose unverified iat
// is not earlier than the set's fetch time (minus issuedAtSkew) nor later
// than now (plus issuedAtSkew) may be signed by a key published after that
// fetch (Tikti publishes only its current key, so rotation is not
// pre-announced): it gets 503 + Retry-After so the client retries once the
// next permitted refresh can see the key. Anything else is refused (401): a
// token for another issuer or audience could never be accepted, whatever
// key signed it. These unverified claims only choose between 401 and 503:
// the choice is made after the refresh-or-skip decision, so it neither
// verifies anything nor starts a fetch. A forged token needs this
// validator's own issuer and audience and an iat within the window to earn
// a 503, and it is refused again once a later refresh succeeds.
func (c *keyCache) unknownKid(set *keySet, now time.Time, hint tokenHint, retry time.Duration) error {
	age := now.Sub(set.fetchedAt)
	if hint.ours && !hint.issuedAt.IsZero() &&
		!hint.issuedAt.Before(set.fetchedAt.Add(-issuedAtSkew)) &&
		!hint.issuedAt.After(now.Add(issuedAtSkew)) {
		observeDecision(decisionKidNotYetKnown, age)
		return &auth.UnavailableError{RetryAfter: retry}
	}
	observeDecision(stateFor(age, c.refreshInterval), age)
	return invalid("unknown kid")
}

// lookup returns kid's key; a nil set has none.
func (s *keySet) lookup(kid string) (verificationKey, bool) {
	if s == nil {
		return verificationKey{}, false
	}
	key, ok := s.keys[kid]
	return key, ok
}

func checkAlg(key verificationKey, alg string) (*rsa.PublicKey, error) {
	if key.alg != "" && key.alg != alg {
		return nil, invalid("algorithm does not match key")
	}
	return key.key, nil
}

// usableSetLocked returns the last good set when it is within maxKeySetAge.
func (c *keyCache) usableSetLocked(now time.Time) *keySet {
	if c.set == nil || now.Sub(c.set.fetchedAt) > c.maxKeySetAge {
		return nil
	}
	return c.set
}

// retryAfterLocked is the delay until a refresh may have produced a set.
func (c *keyCache) retryAfterLocked(now time.Time) time.Duration {
	if c.inflight != nil {
		return c.httpTimeout
	}
	wait := c.lastAttempt.Add(c.minRefreshInterval).Sub(now)
	return max(wait, time.Second)
}

// startRefreshLocked joins the in-flight refresh, starts a new one when the
// minimum interval since the last attempt has elapsed, or returns nil.
func (c *keyCache) startRefreshLocked(now time.Time, trigger string) *refreshCall {
	if c.inflight != nil {
		return c.inflight
	}
	if !c.lastAttempt.IsZero() && now.Sub(c.lastAttempt) < c.minRefreshInterval {
		observer().RefreshSkipped(trigger)
		return nil
	}
	call := &refreshCall{done: make(chan struct{})}
	c.inflight = call
	c.lastAttempt = now
	go c.runRefresh(call)
	return call
}

func (c *keyCache) runRefresh(call *refreshCall) {
	ctx, cancel := context.WithTimeout(context.Background(), c.httpTimeout)
	defer cancel()
	keys, outcome := c.fetch(ctx)

	c.mu.Lock()
	now := c.now()
	previous := c.set
	if authoritative(outcome) {
		// A well-formed answer replaces the whole set: keys it no longer
		// publishes stop verifying at once, even when none remain usable.
		c.set = &keySet{keys: keys, fetchedAt: now}
		c.lastAttemptFailed = false
	} else {
		call.err = errors.New(outcome)
		c.lastAttemptFailed = true
	}
	c.mu.Unlock()

	observer().RefreshCompleted(outcome)
	c.logRefresh(outcome, previous, keys, now)

	// Release joiners only after the outcome is observable, and clear
	// inflight last so no new attempt can start before this one is done.
	c.mu.Lock()
	c.inflight = nil
	c.mu.Unlock()
	close(call.done)
}

func (c *keyCache) logRefresh(outcome string, previous *keySet, keys map[string]verificationKey, now time.Time) {
	age := -1.0
	if previous != nil {
		age = now.Sub(previous.fetchedAt).Seconds()
	}
	if outcome == outcomeNoUsableKeys {
		_, removed := diffKids(previous, keys)
		c.logger.Warn("jwks key set has no usable keys",
			"component", "jwks", "outcome", outcome, "key_count", 0, "keys_removed", removed)
		return
	}
	if outcome != outcomeSuccess {
		c.logger.Warn("jwks refresh failed",
			"component", "jwks", "outcome", outcome,
			"key_set_age_seconds", math.Round(age), "key_set_max_age_seconds", c.maxKeySetAge.Seconds(),
			"usable_key_set", previous != nil && age <= c.maxKeySetAge.Seconds())
		return
	}
	added, removed := diffKids(previous, keys)
	if previous == nil || added > 0 || removed > 0 {
		c.logger.Info("jwks key set updated",
			"component", "jwks", "outcome", outcome, "key_count", len(keys),
			"keys_added", added, "keys_removed", removed,
			"previous_key_set_age_seconds", math.Round(age))
	}
}

func diffKids(previous *keySet, keys map[string]verificationKey) (added, removed int) {
	if previous == nil {
		return len(keys), 0
	}
	for kid := range keys {
		if _, ok := previous.keys[kid]; !ok {
			added++
		}
	}
	for kid := range previous.keys {
		if _, ok := keys[kid]; !ok {
			removed++
		}
	}
	return added, removed
}

// fetch downloads and validates the key set. The returned outcome is one of
// the fixed refresh outcome values; no error text leaves this function.
func (c *keyCache) fetch(ctx context.Context) (map[string]verificationKey, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURL, nil)
	if err != nil {
		return nil, outcomeFetchError
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, outcomeFetchTimeout
		}
		return nil, outcomeFetchError
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, outcomeHTTPStatus
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, outcomeFetchTimeout
		}
		return nil, outcomeFetchError
	}
	if len(body) > maxJWKSBytes {
		return nil, outcomeTooLarge
	}
	return parseKeySet(body)
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

type jsonWebKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// authoritative reports whether a refresh outcome is a well-formed answer of
// the key-set owner that replaces the cached set.
func authoritative(outcome string) bool {
	return outcome == outcomeSuccess || outcome == outcomeNoUsableKeys
}

// parseKeySet keeps usable RSA signing keys and skips unusable entries
// (small or malformed keys, other key types, encryption keys, disallowed
// algorithms). A well-formed set without usable keys is authoritative
// (outcomeNoUsableKeys) and empties the cache. A duplicate kid is ambiguous
// and, like an unreadable document, fails the refresh so the previous good
// set stays in force within its bound.
func parseKeySet(body []byte) (map[string]verificationKey, string) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var document struct {
		// Keys is a pointer so an absent or null member (RFC 7517 requires
		// it) is an unreadable document, never an authoritative empty set.
		Keys *[]jsonWebKey `json:"keys"`
	}
	if err := decoder.Decode(&document); err != nil || document.Keys == nil {
		return nil, outcomeInvalidDocument
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, outcomeInvalidDocument
	}
	keys := make(map[string]verificationKey, len(*document.Keys))
	seen := make(map[string]struct{}, len(*document.Keys))
	for _, jwk := range *document.Keys {
		if jwk.Kid != "" {
			if _, dup := seen[jwk.Kid]; dup {
				return nil, outcomeDuplicateKid
			}
			seen[jwk.Kid] = struct{}{}
		}
		key, ok := usableKey(jwk)
		if !ok {
			continue
		}
		keys[jwk.Kid] = key
	}
	if len(keys) == 0 {
		return keys, outcomeNoUsableKeys
	}
	return keys, outcomeSuccess
}

func usableKey(jwk jsonWebKey) (verificationKey, bool) {
	if jwk.Kid == "" || jwk.Kty != ktyRSA || (jwk.Use != "" && jwk.Use != useSignature) {
		return verificationKey{}, false
	}
	if jwk.Alg != "" && !isAllowedAlgorithm(jwk.Alg) {
		return verificationKey{}, false
	}
	pub, err := parseRSAPublicKey(jwk.N, jwk.E)
	if err != nil {
		return verificationKey{}, false
	}
	return verificationKey{key: pub, alg: jwk.Alg}, true
}

func isAllowedAlgorithm(alg string) bool {
	for _, allowed := range allowedAlgorithms {
		if alg == allowed {
			return true
		}
	}
	return false
}

func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil || len(nBytes) == 0 {
		return nil, errors.New("invalid RSA modulus")
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil || len(eBytes) == 0 || len(eBytes) > 4 {
		return nil, errors.New("invalid RSA exponent")
	}
	n := new(big.Int).SetBytes(nBytes)
	if bits := n.BitLen(); bits < minRSAKeyBits || bits > maxRSAKeyBits {
		return nil, errors.New("RSA modulus size outside the allowed range")
	}
	e := new(big.Int).SetBytes(eBytes).Int64()
	if e < 3 || e%2 == 0 || e > math.MaxInt32 {
		return nil, errors.New("invalid RSA exponent")
	}
	return &rsa.PublicKey{N: n, E: int(e)}, nil
}

func getStringClaim(claims jwt.MapClaims, key string) string {
	if v, ok := claims[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
