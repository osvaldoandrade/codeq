package identitymw

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config matches the subset of configuration used by this repo's middleware.
type Config struct {
	JwksURL     string
	Issuer      string
	Audience    string
	ClockSkew   time.Duration
	HTTPTimeout time.Duration
}

type Validator struct {
	cfg    Config
	client *http.Client

	mu    sync.RWMutex
	keys  map[string]*rsa.PublicKey // kid -> key
	fetch time.Time
}

const jwksRefreshInterval = 5 * time.Minute

// Claims is a lightweight representation of validated JWT claims.
type Claims struct {
	Subject    string
	Email      string
	Issuer     string
	Audience   []string
	ExpiresAt  int64
	IssuedAt   int64
	Scopes     []string
	EventTypes []string

	Raw map[string]any
}

func (c *Claims) HasScope(scope string) bool {
	if c == nil {
		return false
	}
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func NewValidator(cfg Config) (*Validator, error) {
	if strings.TrimSpace(cfg.JwksURL) == "" {
		return nil, errors.New("jwks url is required")
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 5 * time.Second
	}
	return &Validator{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.HTTPTimeout},
		keys:   map[string]*rsa.PublicKey{},
	}, nil
}

func (v *Validator) Validate(token string) (*Claims, error) {
	h, p, sig, signingInput, err := splitJWT(token)
	if err != nil {
		return nil, err
	}

	kid, alg, err := parseHeader(h)
	if err != nil {
		return nil, err
	}
	if alg != "RS256" {
		return nil, fmt.Errorf("unsupported alg")
	}

	pub, err := v.publicKeyForKID(kid)
	if err != nil {
		return nil, err
	}

	if err := verifyRS256(pub, signingInput, sig); err != nil {
		return nil, fmt.Errorf("invalid signature")
	}

	raw, err := parsePayload(p)
	if err != nil {
		return nil, err
	}
	if err := v.validateRegistered(raw); err != nil {
		return nil, err
	}

	return materializeClaims(raw), nil
}

func (v *Validator) validateRegistered(raw map[string]any) error {
	now := time.Now().UTC().Unix()
	skew := int64(v.cfg.ClockSkew.Seconds())

	if v.cfg.Issuer != "" {
		if iss, _ := raw["iss"].(string); iss != v.cfg.Issuer {
			return fmt.Errorf("invalid issuer")
		}
	}

	if v.cfg.Audience != "" {
		auds := extractAudience(raw["aud"])
		ok := false
		for _, a := range auds {
			if a == v.cfg.Audience {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("invalid audience")
		}
	}

	exp, ok := extractInt64(raw["exp"])
	if !ok {
		return fmt.Errorf("missing exp")
	}
	if now > exp+skew {
		return fmt.Errorf("token expired")
	}

	if iat, ok := extractInt64(raw["iat"]); ok {
		if iat > now+skew {
			return fmt.Errorf("token used before issued")
		}
	}

	return nil
}

func (v *Validator) publicKeyForKID(kid string) (*rsa.PublicKey, error) {
	if strings.TrimSpace(kid) == "" {
		return nil, fmt.Errorf("missing kid")
	}

	v.mu.RLock()
	k, ok := v.keys[kid]
	lastFetch := v.fetch
	v.mu.RUnlock()

	if ok && k != nil && !lastFetch.IsZero() && time.Since(lastFetch) < jwksRefreshInterval {
		return k, nil
	}

	// If we have a cached key but the JWKS cache is stale, try a best-effort refresh and fall back
	// to the cached key on transient failures.
	if ok && k != nil {
		ctx, cancel := context.WithTimeout(context.Background(), v.cfg.HTTPTimeout)
		defer cancel()
		if err := v.refresh(ctx); err != nil {
			return k, nil
		}
		v.mu.RLock()
		nk := v.keys[kid]
		v.mu.RUnlock()
		if nk != nil {
			return nk, nil
		}
		return k, nil
	}

	// Missing key: refresh JWKS (bounded).
	ctx, cancel := context.WithTimeout(context.Background(), v.cfg.HTTPTimeout)
	defer cancel()
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}

	v.mu.RLock()
	k = v.keys[kid]
	v.mu.RUnlock()
	if k == nil {
		return nil, fmt.Errorf("unknown kid")
	}
	return k, nil
}

func (v *Validator) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("jwks fetch failed")
	}

	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return err
	}

	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if strings.ToUpper(k.Kty) != "RSA" {
			continue
		}
		pub, err := rsaPublicKeyFromJWKS(k.N, k.E)
		if err != nil || pub == nil {
			continue
		}
		if strings.TrimSpace(k.Kid) == "" {
			continue
		}
		keys[k.Kid] = pub
	}

	v.mu.Lock()
	v.keys = keys
	v.fetch = time.Now()
	v.mu.Unlock()
	return nil
}

func splitJWT(token string) (header, payload, sig []byte, signingInput string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, nil, "", fmt.Errorf("invalid token format")
	}
	signingInput = parts[0] + "." + parts[1]
	header, err = base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("invalid token header")
	}
	payload, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("invalid token payload")
	}
	sig, err = base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, nil, "", fmt.Errorf("invalid token signature")
	}
	return header, payload, sig, signingInput, nil
}

func parseHeader(b []byte) (kid string, alg string, err error) {
	var h map[string]any
	if err := json.Unmarshal(b, &h); err != nil {
		return "", "", fmt.Errorf("invalid header")
	}
	kid, _ = h["kid"].(string)
	alg, _ = h["alg"].(string)
	return kid, alg, nil
}

func parsePayload(b []byte) (map[string]any, error) {
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("invalid claims")
	}
	return p, nil
}

func verifyRS256(pub *rsa.PublicKey, signingInput string, sig []byte) error {
	h := sha256.Sum256([]byte(signingInput))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig)
}

func rsaPublicKeyFromJWKS(nB64, eB64 string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}

	n := new(big.Int).SetBytes(nb)
	e := 0
	for _, b := range eb {
		e = e<<8 + int(b)
	}
	if n.Sign() <= 0 || e <= 0 {
		return nil, fmt.Errorf("invalid jwk")
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}

func extractAudience(v any) []string {
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		return []string{x}
	case []any:
		var out []string
		for _, it := range x {
			if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return x
	default:
		return nil
	}
}

func extractInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func extractStringSlice(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, it := range x {
			if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func materializeClaims(raw map[string]any) *Claims {
	c := &Claims{
		Raw: raw,
	}
	if sub, _ := raw["sub"].(string); sub != "" {
		c.Subject = sub
	}
	if email, _ := raw["email"].(string); email != "" {
		c.Email = email
	}
	if iss, _ := raw["iss"].(string); iss != "" {
		c.Issuer = iss
	}
	c.Audience = extractAudience(raw["aud"])
	if exp, ok := extractInt64(raw["exp"]); ok {
		c.ExpiresAt = exp
	}
	if iat, ok := extractInt64(raw["iat"]); ok {
		c.IssuedAt = iat
	}

	// Common OAuth-style "scope" claim (space-separated string).
	if scopeStr, _ := raw["scope"].(string); strings.TrimSpace(scopeStr) != "" {
		c.Scopes = strings.Fields(scopeStr)
	} else {
		c.Scopes = extractStringSlice(raw["scopes"])
	}
	c.EventTypes = extractStringSlice(raw["eventTypes"])
	return c
}
