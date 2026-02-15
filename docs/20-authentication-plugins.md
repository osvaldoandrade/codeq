# Authentication Plugins

CodeQ uses a plugin-based authentication system that allows you to implement custom authentication providers. This decouples CodeQ from any specific authentication service and makes it adaptable to different environments.

## Overview

The authentication plugin architecture consists of:

1. **Interface**: A common interface (`pkg/auth/interface.go`) that all authentication plugins must implement
2. **Default Plugin**: A JWKS-based implementation (`pkg/auth/jwks/`) that validates JWT tokens using JSON Web Key Sets
3. **Middleware**: The middleware layer uses the plugin interface, making it independent of the authentication implementation

## Using the Default JWKS Plugin

The default JWKS plugin validates JWT tokens signed with RSA keys. It's suitable for most OAuth2/OIDC-based authentication systems.

### Configuration

Configure the JWKS plugin through environment variables or config file:

```yaml
# For producer authentication
identityJwksUrl: https://your-auth-server.com/.well-known/jwks.json
identityIssuer: https://your-auth-server.com
identityAudience: codeq-producer

# For worker authentication
workerJwksUrl: https://your-auth-server.com/.well-known/jwks.json
workerIssuer: https://your-auth-server.com
workerAudience: codeq-worker

# Clock skew tolerance
allowedClockSkewSeconds: 60
```

### Required Token Claims

**Producer tokens** must include:
- `iss`: Issuer identifier (must match `identityIssuer`)
- `aud`: Audience (must include `identityAudience`)
- `sub`: Subject (user identifier)
- `exp`: Expiration time
- `iat`: Issued at time
- `email`: User email (optional, falls back to `sub`)
- `role`: User role (optional, used for admin checks)

**Worker tokens** must include all producer claims plus:
- `eventTypes`: Array of event types the worker can process
- `scope`: Space-separated list of permissions (e.g., "codeq:claim codeq:result")

### Token Validation

The JWKS plugin:
1. Fetches public keys from the JWKS endpoint
2. Validates the token signature using the key identified by `kid` in the token header
3. Verifies issuer, audience, and expiration
4. Caches keys for 5 minutes to reduce network calls

## Creating a Custom Plugin

You can implement your own authentication plugin to integrate with different auth systems (API keys, mutual TLS, custom tokens, etc.).

### Step 1: Implement the Validator Interface

Create a new package (e.g., `pkg/auth/custom/`) and implement the `auth.Validator` interface:

```go
package custom

import (
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

type CustomValidator struct {
	// Your validator state
}

func NewValidator(cfg auth.Config) (auth.Validator, error) {
	// Initialize your validator
	return &CustomValidator{}, nil
}

func (v *CustomValidator) Validate(token string) (*auth.Claims, error) {
	// Implement your validation logic
	// Return auth.Claims with user/worker information
	
	return &auth.Claims{
		Subject:    "user-id",
		Email:      "user@example.com",
		Issuer:     "your-auth-system",
		Audience:   []string{"codeq"},
		ExpiresAt:  expiryTime,
		IssuedAt:   issuedTime,
		Scopes:     []string{"codeq:claim", "codeq:result"},
		EventTypes: []string{"*"},
		Raw:        map[string]interface{}{}, // Additional claims
	}, nil
}
```

### Step 2: Use Your Plugin in Middleware

Modify the middleware to use your custom plugin:

```go
// In internal/middleware/auth.go
import "github.com/yourorg/codeq/pkg/auth/custom"

func newProducerValidator(cfg *config.Config) (auth.Validator, error) {
	return custom.NewValidator(auth.Config{
		// Your config parameters
	})
}
```

### Step 3: Test Your Plugin

Create comprehensive tests for your plugin:

```go
package custom

import (
	"testing"
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

func TestCustomValidator(t *testing.T) {
	validator, err := NewValidator(auth.Config{})
	if err != nil {
		t.Fatal(err)
	}
	
	claims, err := validator.Validate("your-test-token")
	if err != nil {
		t.Fatal(err)
	}
	
	// Assert claims are correct
}
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

### auth.Config

```go
type Config struct {
	JwksURL     string        // JWKS endpoint URL
	Issuer      string        // Expected issuer
	Audience    string        // Expected audience
	ClockSkew   time.Duration // Clock skew tolerance
	HTTPTimeout time.Duration // HTTP client timeout
}
```

Configuration for creating validators. Different plugins may use different fields.

## Migration from identity-middleware

If you're upgrading from a version that used the `codecompany/identity-middleware` dependency:

1. **No configuration changes needed**: The JWKS plugin uses the same configuration variables
2. **Token format unchanged**: Your existing tokens will continue to work
3. **Backward compatible**: The Claims structure is identical

The only difference is that CodeQ no longer depends on the private `identity-middleware` package, making it usable without access to that repository.

## Examples

See `pkg/auth/jwks/validator_test.go` for a complete example of JWKS token validation.

For a minimal custom plugin example, see the "Creating a Custom Plugin" section above.

## Security Considerations

When implementing a custom plugin:

1. **Validate all inputs**: Never trust token data without validation
2. **Check expiration**: Always verify token expiration times
3. **Use constant-time comparison**: For secrets, use `crypto/subtle.ConstantTimeCompare`
4. **Cache carefully**: Balance performance with freshness of validation data
5. **Log failures**: Log authentication failures for security monitoring
6. **Rate limit**: Consider rate limiting validation attempts

## Troubleshooting

### Token validation fails with "invalid issuer"

Ensure your `identityIssuer` or `workerIssuer` configuration exactly matches the `iss` claim in the token.

### Token validation fails with "invalid audience"

The token's `aud` claim must include the configured audience (`identityAudience` or `workerAudience`).

### "Key not found in JWKS"

The `kid` in your token header doesn't match any key in the JWKS endpoint. Ensure:
1. The JWKS URL is correct
2. The key used to sign the token is published in the JWKS
3. The `kid` in the token matches the `kid` in the JWKS

### Performance issues

The JWKS plugin caches keys for 5 minutes. If you need different caching behavior, implement a custom plugin with your preferred caching strategy.
