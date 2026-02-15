# Custom Authentication Plugin Example

This example demonstrates how to create a custom authentication plugin for CodeQ.

## Simple API Key Plugin

This plugin validates requests using API keys instead of JWT tokens.

```go
package apikey

import (
	"errors"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

// Validator validates API keys
type Validator struct {
	validKeys map[string]UserInfo
}

// UserInfo stores information about an API key owner
type UserInfo struct {
	UserID     string
	Email      string
	Scopes     []string
	EventTypes []string
	IsAdmin    bool
}

// NewValidator creates a new API key validator
func NewValidator(cfg auth.Config) (auth.Validator, error) {
	// In a real implementation, you would load keys from a database or config
	// This is just an example with hardcoded keys
	validKeys := map[string]UserInfo{
		"producer-key-123": {
			UserID: "producer-1",
			Email:  "producer@example.com",
			Scopes: []string{"codeq:create"},
		},
		"worker-key-456": {
			UserID:     "worker-1",
			Email:      "worker@example.com",
			Scopes:     []string{"codeq:claim", "codeq:result"},
			EventTypes: []string{"TASK_A", "TASK_B"},
		},
		"admin-key-789": {
			UserID:  "admin-1",
			Email:   "admin@example.com",
			Scopes:  []string{"codeq:admin"},
			IsAdmin: true,
		},
	}

	return &Validator{
		validKeys: validKeys,
	}, nil
}

// Validate validates an API key
func (v *Validator) Validate(token string) (*auth.Claims, error) {
	// Remove "Bearer " prefix if present
	token = strings.TrimPrefix(token, "Bearer ")
	token = strings.TrimSpace(token)

	userInfo, ok := v.validKeys[token]
	if !ok {
		return nil, errors.New("invalid API key")
	}

	now := time.Now()
	claims := &auth.Claims{
		Subject:    userInfo.UserID,
		Email:      userInfo.Email,
		Issuer:     "apikey-plugin",
		Audience:   []string{"codeq"},
		ExpiresAt:  now.Add(24 * time.Hour), // API keys don't really expire, but we set a future date
		IssuedAt:   now,
		Scopes:     userInfo.Scopes,
		EventTypes: userInfo.EventTypes,
		Raw:        make(map[string]interface{}),
	}

	if userInfo.IsAdmin {
		claims.Raw["role"] = "ADMIN"
	}

	return claims, nil
}
```

## Using the Custom Plugin

To use this plugin in your CodeQ instance:

1. **Create the plugin package** in your fork or custom build:
   ```bash
   mkdir -p pkg/auth/apikey
   # Add the code above to pkg/auth/apikey/validator.go
   ```

2. **Update middleware to use the plugin**:
   ```go
   // In internal/middleware/auth.go
   import "github.com/yourorg/codeq/pkg/auth/apikey"
   
   func newProducerValidator(cfg *config.Config) (auth.Validator, error) {
       return apikey.NewValidator(auth.Config{})
   }
   ```

3. **Test the plugin**:
   ```go
   package apikey
   
   import (
       "testing"
       "github.com/osvaldoandrade/codeq/pkg/auth"
   )
   
   func TestAPIKeyValidator(t *testing.T) {
       validator, err := NewValidator(auth.Config{})
       if err != nil {
           t.Fatal(err)
       }
       
       // Test valid key
       claims, err := validator.Validate("producer-key-123")
       if err != nil {
           t.Fatalf("expected valid key: %v", err)
       }
       
       if claims.Subject != "producer-1" {
           t.Errorf("expected subject producer-1, got %s", claims.Subject)
       }
       
       // Test invalid key
       _, err = validator.Validate("invalid-key")
       if err == nil {
           t.Error("expected error for invalid key")
       }
   }
   ```

## Real-World Plugin Examples

### Database-backed API Keys

```go
func NewValidator(cfg auth.Config) (auth.Validator, error) {
    db, err := sql.Open("postgres", cfg.DatabaseURL)
    if err != nil {
        return nil, err
    }
    
    return &Validator{
        db: db,
    }, nil
}

func (v *Validator) Validate(token string) (*auth.Claims, error) {
    var userInfo UserInfo
    err := v.db.QueryRow(
        "SELECT user_id, email, scopes, event_types FROM api_keys WHERE key = $1 AND active = true",
        token,
    ).Scan(&userInfo.UserID, &userInfo.Email, &userInfo.Scopes, &userInfo.EventTypes)
    
    if err != nil {
        return nil, errors.New("invalid API key")
    }
    
    // Build and return claims...
}
```

### OAuth2 Introspection

```go
func (v *Validator) Validate(token string) (*auth.Claims, error) {
    // Call OAuth2 introspection endpoint
    resp, err := http.PostForm(v.introspectionURL, url.Values{
        "token": {token},
    })
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    
    var introspection struct {
        Active    bool     `json:"active"`
        Subject   string   `json:"sub"`
        Email     string   `json:"email"`
        Scope     string   `json:"scope"`
        ExpiresAt int64    `json:"exp"`
    }
    
    if err := json.NewDecoder(resp.Body).Decode(&introspection); err != nil {
        return nil, err
    }
    
    if !introspection.Active {
        return nil, errors.New("token is not active")
    }
    
    // Build and return claims...
}
```

### mTLS Certificate Authentication

```go
func (v *Validator) Validate(certPEM string) (*auth.Claims, error) {
    block, _ := pem.Decode([]byte(certPEM))
    if block == nil {
        return nil, errors.New("failed to parse certificate PEM")
    }
    
    cert, err := x509.ParseCertificate(block.Bytes)
    if err != nil {
        return nil, err
    }
    
    // Verify certificate chain
    opts := x509.VerifyOptions{
        Roots: v.rootCAs,
    }
    
    if _, err := cert.Verify(opts); err != nil {
        return nil, err
    }
    
    // Extract user info from certificate
    claims := &auth.Claims{
        Subject: cert.Subject.CommonName,
        Email:   cert.EmailAddresses[0],
        // ...
    }
    
    return claims, nil
}
```

## Best Practices

1. **Validate early**: Reject invalid tokens as quickly as possible
2. **Cache strategically**: Balance security with performance
3. **Log failures**: Track authentication failures for security monitoring
4. **Use timeouts**: Set reasonable HTTP timeouts for external calls
5. **Handle errors gracefully**: Return clear error messages
6. **Test thoroughly**: Write comprehensive unit and integration tests
7. **Document requirements**: Clearly document what your plugin expects

## Configuration

Extend the `auth.Config` struct if your plugin needs additional configuration:

```go
type Config struct {
    JwksURL     string        // Standard fields
    Issuer      string
    Audience    string
    ClockSkew   time.Duration
    HTTPTimeout time.Duration
    
    // Your custom fields
    DatabaseURL     string
    APIKeyPrefix    string
    EnableCaching   bool
    CacheTTL        time.Duration
}
```

Then use these fields in your `NewValidator` function to configure your plugin.
