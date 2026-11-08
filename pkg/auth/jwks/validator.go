// Package jwks validates RSA-signed JWTs against a remote JSON Web Key Set.
//
// The key set is cached as a whole and refreshed by at most one request at a
// time, never more often than minRefreshInterval, so unauthenticated callers
// cannot amplify traffic to the identity provider. When a refresh fails the
// last successfully fetched set keeps verifying tokens up to maxKeySetAge;
// beyond that, or before the first successful fetch, validation reports
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
	// defaultMinRefreshInterval spaces refresh attempts of any cause:
	// unknown kid, refresh-ahead, retry after a failure, or cold start.
	defaultMinRefreshInterval = 30 * time.Second
	// defaultMaxKeySetAge bounds how long the last successfully fetched key
	// set keeps verifying tokens while refreshes fail. See ADR 0014.
	defaultMaxKeySetAge = 15 * time.Minute

	defaultHTTPTimeout = 5 * time.Second
	maxHTTPTimeout     = 10 * time.Second
	maxRedirects       = 3

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

// refuseUnsafeRedirect bounds redirects and refuses an HTTPS→HTTP downgrade
// of the key-set trust boundary.
func refuseUnsafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many JWKS redirects")
	}
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("JWKS redirect downgrades HTTPS")
	}
	return nil
}

// Validate validates a JWT token. Refusals wrap auth.ErrInvalidToken; a
// missing usable key set returns *auth.UnavailableError. Error messages never
// carry token content, the JWKS URL or transport details.
func (v *Validator) Validate(tokenString string) (*auth.Claims, error) {
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
		key, err := v.keyFor(kid, token.Method.Alg())
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

// keyFor returns the verification key for kid from a usable key set.
//
//   - Known kid in a set younger than maxKeySetAge: returned at once; a set
//     older than refreshInterval also starts one background refresh.
//   - Unknown kid: joins the in-flight refresh or starts one when
//     minRefreshInterval has elapsed since the last attempt; otherwise the
//     kid is negatively cached for the rest of that interval.
//   - No usable set: *auth.UnavailableError.
func (v *Validator) keyFor(kid, alg string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	now := v.now()
	set := v.usableSetLocked(now)
	if set != nil {
		if key, ok := set.keys[kid]; ok {
			age := now.Sub(set.fetchedAt)
			if age >= v.refreshInterval {
				v.startRefreshLocked(now, "refresh_ahead")
			}
			v.mu.Unlock()
			observeDecision(stateFor(age, v.refreshInterval), age)
			return checkAlg(key, alg)
		}
	}
	call := v.startRefreshLocked(now, "unknown_kid")
	if call == nil {
		lastFailed := v.lastAttemptFailed
		retry := v.retryAfterLocked(now)
		v.mu.Unlock()
		if set != nil && !lastFailed {
			// Absent from a set fetched successfully within the interval.
			observeDecision(stateFor(now.Sub(set.fetchedAt), v.refreshInterval), now.Sub(set.fetchedAt))
			return nil, invalid("unknown kid")
		}
		observeDecision(decisionUnavailable, 0)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}
	v.mu.Unlock()

	<-call.done

	v.mu.Lock()
	now = v.now()
	set = v.usableSetLocked(now)
	retry := v.retryAfterLocked(now)
	v.mu.Unlock()
	if set == nil {
		observeDecision(decisionUnavailable, 0)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}
	age := now.Sub(set.fetchedAt)
	if key, ok := set.keys[kid]; ok {
		observeDecision(stateFor(age, v.refreshInterval), age)
		return checkAlg(key, alg)
	}
	if call.err != nil {
		// The refresh that could have published this kid failed: the token
		// can be neither verified nor refused yet.
		observeDecision(decisionUnavailable, age)
		return nil, &auth.UnavailableError{RetryAfter: retry}
	}
	observeDecision(stateFor(age, v.refreshInterval), age)
	return nil, invalid("unknown kid")
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
