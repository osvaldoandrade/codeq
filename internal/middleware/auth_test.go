package middleware

import (
	"bytes"
	"context"
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

	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

type testEnv struct {
	cfg     *config.Config
	jwksSrv *httptest.Server
	idSrv   *httptest.Server
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
		eBytes := []byte{0x01, 0x00, 0x01}
		e := base64.RawURLEncoding.EncodeToString(eBytes)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{"kty": "RSA", "kid": "kid-1", "n": n, "e": e}},
		})
	}))
	idSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/accounts/lookup" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"users": []map[string]any{{"localId": "u1", "email": "u@codeq.local", "role": "ADMIN"}},
		})
	}))
	cfg := &config.Config{
		Env:                     "test",
		WorkerJwksURL:           jwksSrv.URL,
		WorkerAudience:          "codeq-worker",
		WorkerIssuer:            "codeq-test",
		AllowedClockSkewSeconds: 60,
		IdentityServiceURL:      idSrv.URL,
		IdentityServiceApiKey:   "k",
	}
	return &testEnv{cfg: cfg, jwksSrv: jwksSrv, idSrv: idSrv, privKey: privKey}
}

func (e *testEnv) close() {
	e.jwksSrv.Close()
	e.idSrv.Close()
}

func signWorkerJWT(t *testing.T, key *rsa.PrivateKey, kid, iss, aud, sub string, scopes string, eventTypes []string) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	now := time.Now().Unix()
	payload := map[string]any{
		"iss":        iss,
		"aud":        aud,
		"sub":        sub,
		"exp":        now + 3600,
		"iat":        now - 10,
		"jti":        "jid-1",
		"eventTypes": eventTypes,
		"scope":      scopes,
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	h := enc(header)
	p := enc(payload)
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
	cache := &jwksCache{}
	tok := signWorkerJWT(t, env.privKey, "kid-1", env.cfg.WorkerIssuer, env.cfg.WorkerAudience, "w1", "codeq:claim", []string{"GENERATE_MASTER"})
	claims, err := validateWorkerJWT(context.Background(), env.cfg, cache, "Bearer "+tok)
	if err != nil {
		t.Fatalf("expected valid token: %v", err)
	}
	if claims.Subject != "w1" {
		t.Fatalf("subject mismatch: %s", claims.Subject)
	}
}

func TestWorkerAuthMissingScope(t *testing.T) {
	env := setupEnv(t)
	defer env.close()
	cache := &jwksCache{}
	tok := signWorkerJWT(t, env.privKey, "kid-1", env.cfg.WorkerIssuer, env.cfg.WorkerAudience, "w1", "", []string{"GENERATE_MASTER"})
	_, err := validateWorkerJWT(context.Background(), env.cfg, cache, "Bearer "+tok)
	if err == nil {
		t.Fatalf("expected error for missing scope")
	}
}

func TestWorkerAuthInvalidAudience(t *testing.T) {
	env := setupEnv(t)
	defer env.close()
	cache := &jwksCache{}
	tok := signWorkerJWT(t, env.privKey, "kid-1", env.cfg.WorkerIssuer, "wrong", "w1", "codeq:claim", []string{"GENERATE_MASTER"})
	_, err := validateWorkerJWT(context.Background(), env.cfg, cache, "Bearer "+tok)
	if err == nil {
		t.Fatalf("expected error for invalid audience")
	}
}

func TestProducerAuthLookup(t *testing.T) {
	env := setupEnv(t)
	defer env.close()
	user, err := validateProducerToken(context.Background(), env.cfg, "Bearer token")
	if err != nil {
		t.Fatalf("expected valid producer token: %v", err)
	}
	if user.Role != "ADMIN" {
		t.Fatalf("expected role ADMIN, got %s", user.Role)
	}
}

func TestProducerAuthLookupFailure(t *testing.T) {
	env := setupEnv(t)
	env.idSrv.Close()
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid"))
	}))
	defer failSrv.Close()
	env.cfg.IdentityServiceURL = failSrv.URL
	_, err := validateProducerToken(context.Background(), env.cfg, "Bearer token")
	if err == nil {
		t.Fatalf("expected error for invalid token lookup")
	}
}

func TestRequireWorkerScopeMiddleware(t *testing.T) {
	env := setupEnv(t)
	defer env.close()
	cache := &jwksCache{}
	tok := signWorkerJWT(t, env.privKey, "kid-1", env.cfg.WorkerIssuer, env.cfg.WorkerAudience, "w1", "codeq:claim", []string{"GENERATE_MASTER"})
	claims, err := validateWorkerJWT(context.Background(), env.cfg, cache, "Bearer "+tok)
	if err != nil {
		t.Fatalf("token invalid: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", bytes.NewBuffer(nil))
	ctx.Set("workerClaims", claims)

	mw := RequireWorkerScope("codeq:claim")
	mw(ctx)
	if rec.Code != 0 && rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
}
