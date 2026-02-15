package jwks

import (
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
)

func TestJWKSValidator(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(privKey.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": "test-key-1",
					"n":   n,
					"e":   e,
				},
			},
		})
	}))
	defer jwksServer.Close()

	validator, err := NewValidator(auth.Config{
		JwksURL:     jwksServer.URL,
		Issuer:      "test-issuer",
		Audience:    "test-audience",
		ClockSkew:   60 * time.Second,
		HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create validator: %v", err)
	}

	now := time.Now().Unix()
	token := signToken(t, privKey, "test-key-1", map[string]any{
		"iss":        "test-issuer",
		"aud":        "test-audience",
		"sub":        "test-user",
		"exp":        now + 3600,
		"iat":        now,
		"email":      "test@example.com",
		"scope":      "read write",
		"eventTypes": []string{"EVENT_A", "EVENT_B"},
	})

	claims, err := validator.Validate(token)
	if err != nil {
		t.Fatalf("failed to validate token: %v", err)
	}

	if claims.Subject != "test-user" {
		t.Errorf("expected subject 'test-user', got '%s'", claims.Subject)
	}
	if claims.Email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got '%s'", claims.Email)
	}
	if claims.Issuer != "test-issuer" {
		t.Errorf("expected issuer 'test-issuer', got '%s'", claims.Issuer)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != "test-audience" {
		t.Errorf("expected audience ['test-audience'], got %v", claims.Audience)
	}
	if len(claims.Scopes) != 2 || claims.Scopes[0] != "read" || claims.Scopes[1] != "write" {
		t.Errorf("expected scopes ['read', 'write'], got %v", claims.Scopes)
	}
	if len(claims.EventTypes) != 2 || claims.EventTypes[0] != "EVENT_A" || claims.EventTypes[1] != "EVENT_B" {
		t.Errorf("expected eventTypes ['EVENT_A', 'EVENT_B'], got %v", claims.EventTypes)
	}
}

func TestJWKSValidatorInvalidIssuer(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(privKey.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": "test-key-1",
					"n":   n,
					"e":   e,
				},
			},
		})
	}))
	defer jwksServer.Close()

	validator, err := NewValidator(auth.Config{
		JwksURL:     jwksServer.URL,
		Issuer:      "test-issuer",
		Audience:    "test-audience",
		ClockSkew:   60 * time.Second,
		HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create validator: %v", err)
	}

	now := time.Now().Unix()
	token := signToken(t, privKey, "test-key-1", map[string]any{
		"iss": "wrong-issuer",
		"aud": "test-audience",
		"sub": "test-user",
		"exp": now + 3600,
		"iat": now,
	})

	_, err = validator.Validate(token)
	if err == nil {
		t.Fatal("expected error for invalid issuer, got nil")
	}
}

func TestJWKSValidatorInvalidAudience(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(privKey.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": "test-key-1",
					"n":   n,
					"e":   e,
				},
			},
		})
	}))
	defer jwksServer.Close()

	validator, err := NewValidator(auth.Config{
		JwksURL:     jwksServer.URL,
		Issuer:      "test-issuer",
		Audience:    "test-audience",
		ClockSkew:   60 * time.Second,
		HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create validator: %v", err)
	}

	now := time.Now().Unix()
	token := signToken(t, privKey, "test-key-1", map[string]any{
		"iss": "test-issuer",
		"aud": "wrong-audience",
		"sub": "test-user",
		"exp": now + 3600,
		"iat": now,
	})

	_, err = validator.Validate(token)
	if err == nil {
		t.Fatal("expected error for invalid audience, got nil")
	}
}

func TestJWKSValidatorExpiredToken(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(privKey.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"kid": "test-key-1",
					"n":   n,
					"e":   e,
				},
			},
		})
	}))
	defer jwksServer.Close()

	validator, err := NewValidator(auth.Config{
		JwksURL:     jwksServer.URL,
		Issuer:      "test-issuer",
		Audience:    "test-audience",
		ClockSkew:   1 * time.Second,
		HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("failed to create validator: %v", err)
	}

	now := time.Now().Unix()
	token := signToken(t, privKey, "test-key-1", map[string]any{
		"iss": "test-issuer",
		"aud": "test-audience",
		"sub": "test-user",
		"exp": now - 3600, // Expired 1 hour ago
		"iat": now - 7200,
	})

	_, err = validator.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
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
