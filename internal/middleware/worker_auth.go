package middleware

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
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

	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

type WorkerClaims struct {
	Issuer      string   `json:"iss"`
	Audience    any      `json:"aud"`
	Subject     string   `json:"sub"`
	ExpiresAt   int64    `json:"exp"`
	IssuedAt    int64    `json:"iat"`
	JWTID       string   `json:"jti"`
	EventTypes  []string `json:"eventTypes"`
	Scope       string   `json:"scope"`
	WorkerGroup string   `json:"workerGroup"`
}

type jwkSet struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

type jwksCache struct {
	mu      sync.RWMutex
	fetched time.Time
	keys    jwkSet
}

func WorkerAuthMiddleware(cfg *config.Config) gin.HandlerFunc {
	cache := &jwksCache{}
	return func(c *gin.Context) {
		claims, err := validateWorkerJWT(c.Request.Context(), cfg, cache, c.GetHeader("Authorization"))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		c.Set("workerClaims", claims)
		c.Next()
	}
}

func validateWorkerJWT(ctx context.Context, cfg *config.Config, cache *jwksCache, authHeader string) (*WorkerClaims, error) {
	if authHeader == "" {
		return nil, errors.New("missing Authorization header")
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return nil, errors.New("invalid Authorization format")
	}
	token := parts[1]

	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return nil, errors.New("invalid token format")
	}
	headSeg, payloadSeg, sigSeg := segments[0], segments[1], segments[2]

	headJSON, err := base64.RawURLEncoding.DecodeString(headSeg)
	if err != nil {
		return nil, errors.New("invalid token header")
	}
	var head struct {
		Kid string `json:"kid"`
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headJSON, &head); err != nil {
		return nil, errors.New("invalid token header")
	}
	if head.Alg != "RS256" {
		return nil, errors.New("unsupported alg")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadSeg)
	if err != nil {
		return nil, errors.New("invalid token payload")
	}
	var claims WorkerClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, errors.New("invalid token payload")
	}

	if claims.Issuer == "" || cfg.WorkerIssuer == "" || claims.Issuer != cfg.WorkerIssuer {
		return nil, errors.New("invalid issuer")
	}
	if claims.Subject == "" {
		return nil, errors.New("missing sub")
	}
	if claims.JWTID == "" {
		return nil, errors.New("missing jti")
	}
	if claims.ExpiresAt <= 0 || claims.IssuedAt <= 0 {
		return nil, errors.New("missing iat/exp")
	}
	now := time.Now().Unix()
	skew := int64(cfg.AllowedClockSkewSeconds)
	if claims.ExpiresAt < now-skew {
		return nil, errors.New("token expired")
	}
	if claims.IssuedAt > now+skew {
		return nil, errors.New("token issued in future")
	}
	if !audHas(claims.Audience, cfg.WorkerAudience) {
		return nil, errors.New("invalid audience")
	}
	if len(claims.EventTypes) == 0 {
		return nil, errors.New("missing eventTypes")
	}
	if strings.TrimSpace(claims.Scope) == "" {
		return nil, errors.New("missing scope")
	}

	keys, err := fetchJWKS(ctx, cfg, cache)
	if err != nil {
		return nil, err
	}
	pub, err := findRSAPublicKey(keys, head.Kid)
	if err != nil {
		return nil, err
	}
	if err := verifyRS256(pub, headSeg+"."+payloadSeg, sigSeg); err != nil {
		return nil, errors.New("invalid signature")
	}

	return &claims, nil
}

func GetWorkerClaims(c *gin.Context) (*WorkerClaims, bool) {
	v, ok := c.Get("workerClaims")
	if !ok {
		return nil, false
	}
	claims, ok := v.(*WorkerClaims)
	return claims, ok
}

func audHas(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, it := range v {
			if s, ok := it.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func fetchJWKS(ctx context.Context, cfg *config.Config, cache *jwksCache) (jwkSet, error) {
	cache.mu.RLock()
	if time.Since(cache.fetched) < 5*time.Minute && len(cache.keys.Keys) > 0 {
		keys := cache.keys
		cache.mu.RUnlock()
		return keys, nil
	}
	cache.mu.RUnlock()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.WorkerJwksURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return jwkSet{}, fmt.Errorf("jwks fetch error")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jwkSet{}, fmt.Errorf("jwks non-200")
	}
	b, _ := io.ReadAll(resp.Body)
	var keys jwkSet
	if err := json.Unmarshal(b, &keys); err != nil {
		return jwkSet{}, fmt.Errorf("jwks decode error")
	}

	cache.mu.Lock()
	cache.keys = keys
	cache.fetched = time.Now()
	cache.mu.Unlock()

	return keys, nil
}

func findRSAPublicKey(keys jwkSet, kid string) (map[string]string, error) {
	for _, k := range keys.Keys {
		if k.Kty == "RSA" && k.Kid == kid {
			return map[string]string{"n": k.N, "e": k.E}, nil
		}
	}
	return nil, errors.New("kid not found")
}

func verifyRS256(pub map[string]string, signingInput string, sigSeg string) error {
	sig, err := base64.RawURLEncoding.DecodeString(sigSeg)
	if err != nil {
		return err
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(pub["n"])
	if err != nil {
		return err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(pub["e"])
	if err != nil {
		return err
	}

	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	if n.Sign() == 0 || e == 0 {
		return errors.New("invalid key")
	}
	pubKey := rsa.PublicKey{N: n, E: e}
	hashed := sha256.Sum256([]byte(signingInput))
	return rsa.VerifyPKCS1v15(&pubKey, crypto.SHA256, hashed[:], sig)
}
