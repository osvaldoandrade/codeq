package middleware

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/auth"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/jwks" // Register JWKS provider
	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

type testEnv struct {
	cfg     *config.Config
	jwksSrv *httptest.Server
	privKey *rsa.PrivateKey
}

func setupEnv(t *testing.T) *testEnv {
	t.Helper()
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key gen: %v", err)
	}
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(privKey.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{"kty": "RSA", "kid": "kid-1", "n": n, "e": e}},
		})
	}))
	cfg := &config.Config{
		Env:                     "test",
		WorkerJwksURL:           jwksSrv.URL,
		WorkerAudience:          "codeq-worker",
		WorkerIssuer:            "codeq-test",
		AllowedClockSkewSeconds: 60,
		IdentityJwksURL:         jwksSrv.URL,
		IdentityIssuer:          "codeq-test",
		IdentityAudience:        "codeq-producer",
	}
	
	// Setup auth providers config (normally done by LoadConfig)
	cfg.ProducerAuthProvider = "jwks"
	cfg.ProducerAuthConfig, _ = json.Marshal(map[string]interface{}{
		"jwksUrl":     cfg.IdentityJwksURL,
		"issuer":      cfg.IdentityIssuer,
		"audience":    cfg.IdentityAudience,
		"clockSkew":   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
		"httpTimeout": 5 * time.Second,
	})
	cfg.WorkerAuthProvider = "jwks"
	cfg.WorkerAuthConfig, _ = json.Marshal(map[string]interface{}{
		"jwksUrl":     cfg.WorkerJwksURL,
		"issuer":      cfg.WorkerIssuer,
		"audience":    cfg.WorkerAudience,
		"clockSkew":   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
		"httpTimeout": 5 * time.Second,
	})
	
	return &testEnv{cfg: cfg, jwksSrv: jwksSrv, privKey: privKey}
}

func (e *testEnv) close() {
	e.jwksSrv.Close()
}

func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	h := enc(header)
	p := enc(claims)
	signingInput := h + "." + p
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	s := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + s
}

func TestWorkerAuthValid(t *testing.T) {
	env := setupEnv(t)
	defer env.close()
	now := time.Now().Unix()
	tok := signJWT(t, env.privKey, "kid-1", map[string]any{
		"iss":        env.cfg.WorkerIssuer,
		"aud":        env.cfg.WorkerAudience,
		"sub":        "w1",
		"exp":        now + 3600,
		"iat":        now - 10,
		"jti":        "jid-1",
		"eventTypes": []string{"GENERATE_MASTER"},
		"scope":      "codeq:claim",
	})

	// Create validator using config
	validator, err := auth.NewValidator(auth.ProviderConfig{
		Type:   env.cfg.WorkerAuthProvider,
		Config: env.cfg.WorkerAuthConfig,
	})
	if err != nil {
		t.Fatalf("validator init: %v", err)
	}
	claims, err := validateBearer(validator, "Bearer "+tok)
	if err != nil {
		t.Fatalf("expected valid token: %v", err)
	}
	if claims.Subject != "w1" {
		t.Fatalf("subject mismatch: %s", claims.Subject)
	}
	if len(claims.EventTypes) == 0 {
		t.Fatalf("expected eventTypes")
	}
}

func TestWorkerAuthMissingScope(t *testing.T) {
	env := setupEnv(t)
	defer env.close()
	now := time.Now().Unix()
	tok := signJWT(t, env.privKey, "kid-1", map[string]any{
		"iss":        env.cfg.WorkerIssuer,
		"aud":        env.cfg.WorkerAudience,
		"sub":        "w1",
		"exp":        now + 3600,
		"iat":        now - 10,
		"jti":        "jid-1",
		"eventTypes": []string{"GENERATE_MASTER"},
		"scope":      "",
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBuffer(nil))
	ctx.Request.Header.Set("Authorization", "Bearer "+tok)

	// Create validator using config
	validator, _ := auth.NewValidator(auth.ProviderConfig{
		Type:   env.cfg.WorkerAuthProvider,
		Config: env.cfg.WorkerAuthConfig,
	})
	WorkerAuthMiddleware(validator, nil, env.cfg)(ctx)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for missing scope, got %d", rec.Code)
	}
}

func TestWorkerAuthInvalidAudience(t *testing.T) {
	env := setupEnv(t)
	defer env.close()
	now := time.Now().Unix()
	tok := signJWT(t, env.privKey, "kid-1", map[string]any{
		"iss":        env.cfg.WorkerIssuer,
		"aud":        "wrong",
		"sub":        "w1",
		"exp":        now + 3600,
		"iat":        now - 10,
		"jti":        "jid-1",
		"eventTypes": []string{"GENERATE_MASTER"},
		"scope":      "codeq:claim",
	})

	validator, err := auth.NewValidator(auth.ProviderConfig{
		Type:   env.cfg.WorkerAuthProvider,
		Config: env.cfg.WorkerAuthConfig,
	})
	if err != nil {
		t.Fatalf("validator init: %v", err)
	}
	_, err = validateBearer(validator, "Bearer "+tok)
	if err == nil {
		t.Fatalf("expected error for invalid audience")
	}
}

func TestProducerAuthValid(t *testing.T) {
	env := setupEnv(t)
	defer env.close()
	now := time.Now().Unix()
	tok := signJWT(t, env.privKey, "kid-1", map[string]any{
		"iss":   env.cfg.IdentityIssuer,
		"aud":   env.cfg.IdentityAudience,
		"sub":   "u1",
		"exp":   now + 3600,
		"iat":   now - 10,
		"jti":   "jid-1",
		"email": "u@codeq.local",
		"role":  "ADMIN",
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", bytes.NewBuffer(nil))
	ctx.Request.Header.Set("Authorization", "Bearer "+tok)

	// Create validator using config  
	validator, _ := auth.NewValidator(auth.ProviderConfig{
		Type:   env.cfg.ProducerAuthProvider,
		Config: env.cfg.ProducerAuthConfig,
	})
	AuthMiddleware(validator, env.cfg)(ctx)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("expected producer auth to pass")
	}
	if v, ok := ctx.Get("userEmail"); !ok || v.(string) == "" {
		t.Fatalf("expected userEmail in context")
	}
}
