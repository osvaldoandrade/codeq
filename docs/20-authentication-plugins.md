# Authentication Plugins

CodeQ uses a true plugin-based authentication system with framework-like extensibility. No internal code edits are required to add custom authentication providers.

## Overview

The authentication plugin architecture provides three integration points:

1. **Configuration-based** - Select providers via config file (recommended for operations)
2. **Dependency Injection** - Pass validators at application startup (recommended for embedded use)
3. **Provider Registry** - Register factories globally (recommended for libraries)

## Quick Start

### Using the Default JWKS Plugin

**Option 1: Legacy Config (backward compatible)**
```yaml
# Auto-configured as JWKS provider
identityJwksUrl: https://your-auth-server.com/.well-known/jwks.json
identityIssuer: https://your-auth-server.com
identityAudience: codeq-producer

workerJwksUrl: https://your-auth-server.com/.well-known/jwks.json
workerIssuer: https://your-auth-server.com
workerAudience: codeq-worker
```

**Option 2: Explicit Provider Config (new)**
```yaml
producerAuthProvider: jwks
producerAuthConfig:
  jwksUrl: https://your-auth-server.com/.well-known/jwks.json
  issuer: https://your-auth-server.com
  audience: codeq-producer
  clockSkew: 60s
  httpTimeout: 5s

workerAuthProvider: jwks
workerAuthConfig:
  jwksUrl: https://your-auth-server.com/.well-known/jwks.json
  issuer: https://your-auth-server.com
  audience: codeq-worker
  clockSkew: 60s
  httpTimeout: 5s
```

### Environment Variables

```bash
# Legacy (still supported)
IDENTITY_JWKS_URL=https://your-auth-server.com/.well-known/jwks.json
IDENTITY_ISSUER=https://your-auth-server.com
IDENTITY_AUDIENCE=codeq-producer

# Or explicit provider selection (new)
PRODUCER_AUTH_PROVIDER=jwks
PRODUCER_AUTH_CONFIG='{"jwksUrl":"https://...","issuer":"...","audience":"..."}'
```

## Creating Custom Plugins

### Method 1: Provider Registry (Recommended)

Register your provider globally using the init() pattern:

```go
package myauth

import (
	"encoding/json"
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

type Config struct {
	APIKey string `json:"apiKey"`
}

type Validator struct {
	apiKey string
}

func init() {
	auth.RegisterProvider("myauth", NewValidator)
}

func NewValidator(configJSON json.RawMessage) (auth.Validator, error) {
	var cfg Config
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, err
	}
	return &Validator{apiKey: cfg.APIKey}, nil
}

func (v *Validator) Validate(token string) (*auth.Claims, error) {
	// Your validation logic
	if token == v.apiKey {
		return &auth.Claims{
			Subject: "api-user",
			Scopes:  []string{"codeq:claim"},
		}, nil
	}
	return nil, errors.New("invalid API key")
}
```

**Usage:**
```go
import (
	_ "github.com/yourorg/myauth"  // Registers provider
	"github.com/osvaldoandrade/codeq/pkg/app"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

func main() {
	cfg, _ := config.LoadConfig("config.yaml")
	app, _ := app.NewApplication(cfg)
	// Validators auto-created from config
}
```

**Config:**
```yaml
producerAuthProvider: myauth
producerAuthConfig:
  apiKey: secret123
```

### Method 2: Dependency Injection

Pass validators directly at application startup:

```go
package main

import (
	"github.com/osvaldoandrade/codeq/pkg/app"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

type MyValidator struct{}

func (v *MyValidator) Validate(token string) (*auth.Claims, error) {
	// Your validation logic
	return &auth.Claims{Subject: "user"}, nil
}

func main() {
	cfg, _ := config.LoadConfig("config.yaml")
	
	myValidator := &MyValidator{}
	
	app, err := app.NewApplication(cfg,
		app.WithProducerValidator(myValidator),
		app.WithWorkerValidator(myValidator),
	)
	if err != nil {
		panic(err)
	}
	
	app.SetupMappings(app)
	// Start server...
}
```

**Benefits:**
- No global state
- Full control over lifecycle
- Easy testing with mocks

### Method 3: Configuration Only

For simple cases, just configure existing providers:

```yaml
producerAuthProvider: jwks
producerAuthConfig:
  jwksUrl: https://custom-auth.example.com/jwks
  issuer: https://custom-auth.example.com
  audience: my-app
```

## Plugin Interface Reference

### auth.Validator

```go
type Validator interface {
	Validate(token string) (*Claims, error)
}
```

Validates an authentication token and returns claims or an error.

### auth.Claims

```go
type Claims struct {
	Subject    string                 // User/worker identifier
	Email      string                 // User email
	Issuer     string                 // Token issuer
	Audience   []string               // Intended audiences
	ExpiresAt  time.Time              // Expiration time
	IssuedAt   time.Time              // Issue time
	Scopes     []string               // Permissions
	EventTypes []string               // Worker: allowed event types
	Raw        map[string]interface{} // Additional claims
}
```

### auth.ProviderConfig

```go
type ProviderConfig struct {
	Type   string          // Provider type (e.g., "jwks", "apikey")
	Config json.RawMessage // Provider-specific configuration
}
```

### Registry Functions

```go
// Register a provider factory
func RegisterProvider(providerType string, factory ValidatorFactory)

// Create validator from config
func NewValidator(config ProviderConfig) (Validator, error)

// List registered providers
func ListProviders() []string
```

## Real-World Examples

### OAuth2 Introspection Plugin

```go
package oauth2

import (
	"encoding/json"
	"net/http"
	"net/url"
	
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

type Config struct {
	IntrospectionURL string `json:"introspectionUrl"`
	ClientID         string `json:"clientId"`
	ClientSecret     string `json:"clientSecret"`
}

type Validator struct {
	config Config
	client *http.Client
}

func init() {
	auth.RegisterProvider("oauth2-introspection", NewValidator)
}

func NewValidator(configJSON json.RawMessage) (auth.Validator, error) {
	var cfg Config
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, err
	}
	return &Validator{
		config: cfg,
		client: &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func (v *Validator) Validate(token string) (*auth.Claims, error) {
	resp, err := v.client.PostForm(v.config.IntrospectionURL, url.Values{
		"token":         {token},
		"client_id":     {v.config.ClientID},
		"client_secret": {v.config.ClientSecret},
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	var introspection struct {
		Active bool     `json:"active"`
		Sub    string   `json:"sub"`
		Scope  string   `json:"scope"`
		Exp    int64    `json:"exp"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&introspection); err != nil {
		return nil, err
	}
	
	if !introspection.Active {
		return nil, errors.New("token inactive")
	}
	
	return &auth.Claims{
		Subject:   introspection.Sub,
		Scopes:    strings.Fields(introspection.Scope),
		ExpiresAt: time.Unix(introspection.Exp, 0),
	}, nil
}
```

**Usage:**
```yaml
producerAuthProvider: oauth2-introspection
producerAuthConfig:
  introspectionUrl: https://oauth.example.com/introspect
  clientId: codeq-service
  clientSecret: secret
```

### Database-backed API Key Plugin

See `examples/custom-auth-plugin.md` for a complete implementation.

## Testing Your Plugin

```go
package myauth

import (
	"encoding/json"
	"testing"
	
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

func TestMyAuthPlugin(t *testing.T) {
	cfgJSON, _ := json.Marshal(Config{APIKey: "test-key"})
	
	validator, err := NewValidator(cfgJSON)
	if err != nil {
		t.Fatal(err)
	}
	
	// Test valid token
	claims, err := validator.Validate("test-key")
	if err != nil {
		t.Fatalf("expected valid token: %v", err)
	}
	if claims.Subject != "api-user" {
		t.Errorf("unexpected subject: %s", claims.Subject)
	}
	
	// Test invalid token
	_, err = validator.Validate("wrong-key")
	if err == nil {
		t.Error("expected error for invalid token")
	}
}
```

## Migration from Previous Version

If you implemented a "plugin" in the old system that required editing `newProducerValidator`/`newWorkerValidator`:

**Before (required editing internal code):**
```go
// Had to edit internal/middleware/auth.go
func newProducerValidator(cfg *config.Config) (auth.Validator, error) {
	return myauth.NewValidator(...)  // Edit here
}
```

**After (use registry or DI, no internal edits):**
```go
// Option 1: Register in your package
func init() {
	auth.RegisterProvider("myauth", factory)
}

// Option 2: Use dependency injection
app.NewApplication(cfg, app.WithProducerValidator(myValidator))
```

## Best Practices

1. **Use Registry for Libraries**: If distributing a reusable plugin, use `RegisterProvider` in `init()`
2. **Use DI for Applications**: If building a custom deployment, use `WithProducerValidator` options
3. **Use Config for Operations**: If running standard CodeQ, configure via YAML/env vars
4. **Validate Configuration Early**: Return errors from `NewValidator` if config is invalid
5. **Cache Wisely**: Balance security with performance (JWKS plugin caches keys for 5 minutes)
6. **Handle Errors Gracefully**: Return clear error messages for debugging
7. **Test Thoroughly**: Write unit tests for your validator logic

## Troubleshooting

### "unknown auth provider type"

Your provider isn't registered. Ensure:
1. You're importing the package with `_` (e.g., `import _ "pkg/myauth"`)
2. The init() function calls `auth.RegisterProvider`
3. The provider type in config matches the registered name

### "producer validator not configured"

The application couldn't create a validator. Check:
1. Config has `producerAuthProvider` and `producerAuthConfig` set
2. Or legacy `identityJwksUrl` fields are set for backward compatibility
3. Provider is registered before `NewApplication` is called

### "invalid token" in tests

Ensure test setup properly configures auth:
```go
cfg.ProducerAuthProvider = "jwks"
cfg.ProducerAuthConfig, _ = json.Marshal(map[string]interface{}{
	"jwksUrl": testServer.URL,
	"issuer": "test",
	"audience": "test",
})
```

## See Also

- [Custom Plugin Examples](../examples/custom-auth-plugin.md) - Complete implementations
- [Migration Guide](23-migration-plugin-system.md) - Upgrading from identity-middleware
- [Configuration Reference](14-configuration.md) - All config options
