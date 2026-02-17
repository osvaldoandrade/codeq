package jwks

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

// Config contains JWKS validator configuration
type Config struct {
	JwksURL     string        `json:"jwksUrl"`
	Issuer      string        `json:"issuer"`
	Audience    string        `json:"audience"`
	ClockSkew   time.Duration `json:"clockSkew"`
	HTTPTimeout time.Duration `json:"httpTimeout"`
}

// Validator validates JWT tokens using JWKS
type Validator struct {
	jwksURL     string
	issuer      string
	audience    string
	clockSkew   time.Duration
	httpTimeout time.Duration
	keyCache    map[string]*rsa.PublicKey
	cacheTime   time.Time
	cacheMutex  sync.RWMutex
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
	if cfg.JwksURL == "" {
		return nil, errors.New("jwksURL is required")
	}
	if cfg.Issuer == "" {
		return nil, errors.New("issuer is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("audience is required")
	}

	return &Validator{
		jwksURL:     cfg.JwksURL,
		issuer:      cfg.Issuer,
		audience:    cfg.Audience,
		clockSkew:   cfg.ClockSkew,
		httpTimeout: cfg.HTTPTimeout,
		keyCache:    make(map[string]*rsa.PublicKey),
	}, nil
}

// Validate validates a JWT token
func (v *Validator) Validate(tokenString string) (*auth.Claims, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("missing kid in token header")
		}

		return v.getPublicKey(kid)
	})

	if err != nil {
		return nil, err
	}

	if !token.Valid {
		return nil, errors.New("invalid token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid claims")
	}

	// Validate issuer
	iss, _ := claims["iss"].(string)
	if iss != v.issuer {
		return nil, fmt.Errorf("invalid issuer: %s", iss)
	}

	// Validate audience
	var audiences []string
	switch aud := claims["aud"].(type) {
	case string:
		audiences = []string{aud}
	case []interface{}:
		for _, a := range aud {
			if audStr, ok := a.(string); ok {
				audiences = append(audiences, audStr)
			}
		}
	}

	validAudience := false
	for _, aud := range audiences {
		if aud == v.audience {
			validAudience = true
			break
		}
	}
	if !validAudience {
		return nil, fmt.Errorf("invalid audience: %v", audiences)
	}

	// Validate expiration
	if exp, ok := claims["exp"].(float64); ok {
		expTime := time.Unix(int64(exp), 0)
		if time.Now().After(expTime.Add(v.clockSkew)) {
			return nil, errors.New("token expired")
		}
	}

	// Build auth.Claims
	result := &auth.Claims{
		Subject:  getStringClaim(claims, "sub"),
		Email:    getStringClaim(claims, "email"),
		Issuer:   iss,
		Audience: audiences,
		Raw:      claims,
	}

	if exp, ok := claims["exp"].(float64); ok {
		result.ExpiresAt = time.Unix(int64(exp), 0)
	}
	if iat, ok := claims["iat"].(float64); ok {
		result.IssuedAt = time.Unix(int64(iat), 0)
	}

	// Parse scopes
	if scope, ok := claims["scope"].(string); ok {
		result.Scopes = strings.Fields(scope)
	}

	// Parse eventTypes
	if eventTypes, ok := claims["eventTypes"].([]interface{}); ok {
		for _, et := range eventTypes {
			if etStr, ok := et.(string); ok {
				result.EventTypes = append(result.EventTypes, etStr)
			}
		}
	}

	return result, nil
}

func (v *Validator) getPublicKey(kid string) (*rsa.PublicKey, error) {
	// Check cache (simple time-based cache) with read lock
	v.cacheMutex.RLock()
	if key, ok := v.keyCache[kid]; ok && time.Since(v.cacheTime) < 5*time.Minute {
		v.cacheMutex.RUnlock()
		return key, nil
	}
	v.cacheMutex.RUnlock()

	// Fetch JWKS
	client := &http.Client{Timeout: v.httpTimeout}
	resp, err := client.Get(v.jwksURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read JWKS response: %w", err)
	}

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}

	if err := json.Unmarshal(body, &jwks); err != nil {
		return nil, fmt.Errorf("failed to parse JWKS: %w", err)
	}

	// Find the key
	for _, key := range jwks.Keys {
		if key.Kid == kid && key.Kty == "RSA" {
			pubKey, err := parseRSAPublicKey(key.N, key.E)
			if err != nil {
				return nil, fmt.Errorf("failed to parse RSA key: %w", err)
			}
			// Update cache with write lock
			v.cacheMutex.Lock()
			v.keyCache[kid] = pubKey
			v.cacheTime = time.Now()
			v.cacheMutex.Unlock()
			return pubKey, nil
		}
	}

	return nil, fmt.Errorf("key %s not found in JWKS", kid)
}

func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode n: %w", err)
	}

	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode e: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)

	return &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}, nil
}

func getStringClaim(claims jwt.MapClaims, key string) string {
	if v, ok := claims[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
