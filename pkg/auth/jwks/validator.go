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

// Validator validates JWT tokens using JWKS
type Validator struct {
	jwksURL  string
	issuer   string
	audience string
	client   *http.Client
	parser   *jwt.Parser
	logger   *slog.Logger
	now      func() time.Time

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
}

// NewValidatorFromJSON creates a validator from JSON configuration
func NewValidatorFromJSON(configJSON json.RawMessage) (auth.Validator, error) {
	var cfg Config
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, fmt.Errorf("invalid JWKS config: %w", err)
	}
	return NewValidator(cfg)
}

// NewValidator creates a new JWKS validator
func NewValidator(cfg Config) (auth.Validator, error) {
	return newValidator(cfg)
}

func newValidator(cfg Config) (*Validator, error) {
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

	v := &Validator{
		jwksURL:            cfg.JwksURL,
		issuer:             cfg.Issuer,
		audience:           cfg.Audience,
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
	v.parser = jwt.NewParser(
		jwt.WithValidMethods(allowedAlgorithms),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return v.now() }),
	)
	return v, nil
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
		key, err := v.keyFor(ctx, kid, token.Method.Alg(), unverifiedIssuedAt(claims))
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

// unverifiedIssuedAt returns the token's iat before its signature is
// checked. It only ever selects 503 over 401 for an unknown kid; it never
// contributes to an acceptance.
func unverifiedIssuedAt(claims jwt.MapClaims) time.Time {
	iat, err := claims.GetIssuedAt()
	if err != nil || iat == nil {
		return time.Time{}
	}
	return iat.Time
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
func (v *Validator) keyFor(ctx context.Context, kid, alg string, issuedAt time.Time) (*rsa.PublicKey, error) {
	v.mu.Lock()
	now := v.now()
	set := v.usableSetLocked(now)
	if key, ok := set.lookup(kid); ok {
		return v.knownKidLocked(ctx, set, key, now, kid, alg, issuedAt)
	}
	return v.unknownKidLocked(ctx, set, now, kid, alg, issuedAt)
}

// knownKidLocked decides a kid present in set. It is called with v.mu held
// and releases it.
func (v *Validator) knownKidLocked(ctx context.Context, set *keySet, key verificationKey, now time.Time, kid, alg string, issuedAt time.Time) (*rsa.PublicKey, error) {
	age := now.Sub(set.fetchedAt)
	if age < v.refreshInterval+v.refreshGrace {
		if age >= v.refreshInterval {
			v.startRefreshLocked(now, triggerRefreshAhead)
		}
		v.mu.Unlock()
		observeDecision(stateFor(age, v.refreshInterval), age)
		return checkAlg(key, alg)
	}
	call := v.startRefreshLocked(now, triggerRefreshAhead)
	if call != nil {
		v.mu.Unlock()
		return v.awaitKey(ctx, call, kid, alg, issuedAt)
	}
	// The attempt made within minRefreshInterval did not replace this set,
	// so it failed: Tikti is unreachable and the set stays usable up to
	// maxKeySetAge. Anything else is unexpected and fails closed.
	failed := v.lastAttemptFailed
	retry := v.retryAfterLocked(now)
	v.mu.Unlock()
	if !failed {
		observeDecision(decisionUnavailable, 0)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}
	observeDecision(decisionStale, age)
	return checkAlg(key, alg)
}

// unknownKidLocked decides a kid absent from set (which may be nil). It is
// called with v.mu held and releases it.
func (v *Validator) unknownKidLocked(ctx context.Context, set *keySet, now time.Time, kid, alg string, issuedAt time.Time) (*rsa.PublicKey, error) {
	call := v.startRefreshLocked(now, triggerUnknownKid)
	if call != nil {
		v.mu.Unlock()
		return v.awaitKey(ctx, call, kid, alg, issuedAt)
	}
	lastFailed := v.lastAttemptFailed
	retry := v.retryAfterLocked(now)
	v.mu.Unlock()
	if set != nil && !lastFailed {
		// Absent from a set fetched successfully within the interval.
		return nil, v.unknownKid(set, now, issuedAt, retry)
	}
	observeDecision(decisionUnavailable, 0)
	return nil, &auth.UnavailableError{RetryAfter: retry}
}

// awaitKey waits for call (bounded by the HTTP timeout and ctx) and decides
// kid on the key set it left behind.
func (v *Validator) awaitKey(ctx context.Context, call *refreshCall, kid, alg string, issuedAt time.Time) (*rsa.PublicKey, error) {
	select {
	case <-call.done:
	case <-ctx.Done():
		// The shared refresh continues for the other waiters; this caller
		// is gone and gets no decision, which is never an acceptance.
		v.mu.Lock()
		retry := v.retryAfterLocked(v.now())
		v.mu.Unlock()
		observeDecision(decisionCanceled, 0)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}

	v.mu.Lock()
	now := v.now()
	set := v.usableSetLocked(now)
	retry := v.retryAfterLocked(now)
	v.mu.Unlock()
	if set == nil {
		observeDecision(decisionUnavailable, 0)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}
	age := now.Sub(set.fetchedAt)
	if key, ok := set.keys[kid]; ok {
		// Either the refresh succeeded and still publishes kid, or it failed
		// and the previous set is within maxKeySetAge.
		observeDecision(stateFor(age, v.refreshInterval), age)
		return checkAlg(key, alg)
	}
	if call.err != nil {
		// The refresh that could have published this kid failed: the token
		// can be neither verified nor refused yet.
		observeDecision(decisionUnavailable, age)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}
	return nil, v.unknownKid(set, now, issuedAt, retry)
}

// unknownKid classifies a kid absent from a successfully fetched set. A token
// whose unverified iat is not earlier than the set's fetch time may be signed
// by a key published after that fetch (Tikti publishes only its current key,
// so rotation is not pre-announced): it gets 503 + Retry-After so the client
// retries once the next permitted refresh can see the key. Anything else is
// refused. A forged iat only turns a 401 into a 503: the decision is made
// after the refresh-or-skip choice, so it neither verifies anything nor
// starts a fetch, and it stops applying once a later refresh succeeds.
func (v *Validator) unknownKid(set *keySet, now, issuedAt time.Time, retry time.Duration) error {
	age := now.Sub(set.fetchedAt)
	if !issuedAt.IsZero() &&
		!issuedAt.Before(set.fetchedAt.Add(-issuedAtSkew)) &&
		!issuedAt.After(now.Add(issuedAtSkew)) {
		observeDecision(decisionKidNotYetKnown, age)
		return &auth.UnavailableError{RetryAfter: retry}
	}
	observeDecision(stateFor(age, v.refreshInterval), age)
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
func (v *Validator) usableSetLocked(now time.Time) *keySet {
	if v.set == nil || now.Sub(v.set.fetchedAt) > v.maxKeySetAge {
		return nil
	}
	return v.set
}

// retryAfterLocked is the delay until a refresh may have produced a set.
func (v *Validator) retryAfterLocked(now time.Time) time.Duration {
	if v.inflight != nil {
		return v.httpTimeout
	}
	wait := v.lastAttempt.Add(v.minRefreshInterval).Sub(now)
	return max(wait, time.Second)
}

// startRefreshLocked joins the in-flight refresh, starts a new one when the
// minimum interval since the last attempt has elapsed, or returns nil.
func (v *Validator) startRefreshLocked(now time.Time, trigger string) *refreshCall {
	if v.inflight != nil {
		return v.inflight
	}
	if !v.lastAttempt.IsZero() && now.Sub(v.lastAttempt) < v.minRefreshInterval {
		observer().RefreshSkipped(trigger)
		return nil
	}
	call := &refreshCall{done: make(chan struct{})}
	v.inflight = call
	v.lastAttempt = now
	go v.runRefresh(call)
	return call
}

func (v *Validator) runRefresh(call *refreshCall) {
	ctx, cancel := context.WithTimeout(context.Background(), v.httpTimeout)
	defer cancel()
	keys, outcome := v.fetch(ctx)

	v.mu.Lock()
	now := v.now()
	previous := v.set
	if authoritative(outcome) {
		// A well-formed answer replaces the whole set: keys it no longer
		// publishes stop verifying at once, even when none remain usable.
		v.set = &keySet{keys: keys, fetchedAt: now}
		v.lastAttemptFailed = false
	} else {
		call.err = errors.New(outcome)
		v.lastAttemptFailed = true
	}
	v.mu.Unlock()

	observer().RefreshCompleted(outcome)
	v.logRefresh(outcome, previous, keys, now)

	// Release joiners only after the outcome is observable, and clear
	// inflight last so no new attempt can start before this one is done.
	v.mu.Lock()
	v.inflight = nil
	v.mu.Unlock()
	close(call.done)
}

func (v *Validator) logRefresh(outcome string, previous *keySet, keys map[string]verificationKey, now time.Time) {
	age := -1.0
	if previous != nil {
		age = now.Sub(previous.fetchedAt).Seconds()
	}
	if outcome == outcomeNoUsableKeys {
		_, removed := diffKids(previous, keys)
		v.logger.Warn("jwks key set has no usable keys",
			"component", "jwks", "outcome", outcome, "key_count", 0, "keys_removed", removed)
		return
	}
	if outcome != outcomeSuccess {
		v.logger.Warn("jwks refresh failed",
			"component", "jwks", "outcome", outcome,
			"key_set_age_seconds", math.Round(age), "key_set_max_age_seconds", v.maxKeySetAge.Seconds(),
			"usable_key_set", previous != nil && age <= v.maxKeySetAge.Seconds())
		return
	}
	added, removed := diffKids(previous, keys)
	if previous == nil || added > 0 || removed > 0 {
		v.logger.Info("jwks key set updated",
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
func (v *Validator) fetch(ctx context.Context) (map[string]verificationKey, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, outcomeFetchError
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	resp, err := v.client.Do(req)
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
