# Migration Guide: Removing identity-middleware Dependency

This guide helps you migrate to the new plugin-based authentication system that removes the dependency on the private `codecompany/identity-middleware` package.

## What Changed?

### Before (with identity-middleware)
- CodeQ depended on a private Go module: `github.com/codecompany/identity-middleware`
- Only users with access to this private repository could build CodeQ
- Authentication was tightly coupled to the identity-middleware implementation
- Users needed GitHub credentials to fetch the private dependency

### After (with plugin system)
- CodeQ has a plugin-based authentication interface
- Default JWKS plugin is included, no private dependencies
- Anyone can build and use CodeQ
- Users can implement custom authentication plugins
- Full backward compatibility with existing tokens and configuration

## Who Needs to Migrate?

**You do NOT need to make any changes if:**
- You're using JWT tokens with JWKS validation
- Your tokens are signed with RSA keys
- Your configuration uses `identityJwksUrl`, `identityIssuer`, and `identityAudience`

**You may want to explore custom plugins if:**
- You want to use a different authentication method (API keys, mTLS, etc.)
- You have specific authentication requirements
- You want to integrate with a custom auth system

## Configuration Compatibility

### No Changes Required

All existing configuration variables work exactly as before:

```yaml
# Producer auth - NO CHANGES NEEDED
identityServiceUrl: https://api.storifly.ai
identityJwksUrl: https://api.storifly.ai/v1/.well-known/jwks.json
identityIssuer: https://api.storifly.ai
identityAudience: codeq-producer

# Worker auth - NO CHANGES NEEDED
workerJwksUrl: https://your-jwks-endpoint.com/.well-known/jwks.json
workerIssuer: https://your-auth-server.com
workerAudience: codeq-worker
allowedClockSkewSeconds: 60
```

### Environment Variables

All environment variables remain the same:

```bash
IDENTITY_SERVICE_URL=https://api.storifly.ai
IDENTITY_JWKS_URL=https://api.storifly.ai/v1/.well-known/jwks.json
IDENTITY_ISSUER=https://api.storifly.ai
IDENTITY_AUDIENCE=codeq-producer

WORKER_JWKS_URL=https://your-jwks-endpoint.com/.well-known/jwks.json
WORKER_ISSUER=https://your-auth-server.com
WORKER_AUDIENCE=codeq-worker
ALLOWED_CLOCK_SKEW_SECONDS=60
```

## Token Compatibility

### Producer Tokens

Your existing JWT tokens continue to work without any changes:

```
Existing token format (still supported):
{
  "iss": "https://api.storifly.ai",
  "aud": "codeq-producer",
  "sub": "user-123",
  "email": "user@example.com",
  "exp": 1234567890,
  "iat": 1234567880,
  "role": "ADMIN"
}
```

### Worker Tokens

Worker tokens also remain unchanged:

```
Existing token format (still supported):
{
  "iss": "https://your-auth-server.com",
  "aud": "codeq-worker",
  "sub": "worker-123",
  "exp": 1234567890,
  "iat": 1234567880,
  "eventTypes": ["TASK_A", "TASK_B"],
  "scope": "codeq:claim codeq:result"
}
```

## Building CodeQ

### Before (with private dependency)

```bash
# Required GitHub authentication for private repo
export GOPRIVATE=github.com/codecompany
git config --global url."https://${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"

go build ./cmd/codeq
# Would fail if you didn't have access to codecompany/identity-middleware
```

### After (no private dependencies)

```bash
# No special setup needed - just build
go build ./cmd/codeq
# Works for everyone!
```

## Testing

### Before
```bash
# Tests would fail without access to private dependency
go test ./internal/middleware/...
```

### After
```bash
# Tests work for everyone
go test ./internal/middleware/...
go test ./pkg/auth/jwks/...
go test ./pkg/app/...  # Integration tests
```

All tests now pass without requiring access to private repositories.

## Code Changes (If You Modified CodeQ)

If you've forked CodeQ and made custom modifications, here's what changed:

### Import Changes

**Before:**
```go
import identitymw "github.com/codecompany/identity-middleware"
```

**After:**
```go
import "github.com/osvaldoandrade/codeq/pkg/auth"
import "github.com/osvaldoandrade/codeq/pkg/auth/jwks"
```

### Type Changes

**Before:**
```go
claims, err := validator.Validate(token)
// claims is *identitymw.Claims
```

**After:**
```go
claims, err := validator.Validate(token)
// claims is *auth.Claims
```

The `auth.Claims` structure is identical to `identitymw.Claims`, so no functionality changes.

### Validator Creation

**Before:**
```go
validator, err := identitymw.NewValidator(identitymw.Config{
    JwksURL:     cfg.IdentityJwksURL,
    Issuer:      cfg.IdentityIssuer,
    Audience:    cfg.IdentityAudience,
    ClockSkew:   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
    HTTPTimeout: 5 * time.Second,
})
```

**After:**
```go
validator, err := jwks.NewValidator(auth.Config{
    JwksURL:     cfg.IdentityJwksURL,
    Issuer:      cfg.IdentityIssuer,
    Audience:    cfg.IdentityAudience,
    ClockSkew:   time.Duration(cfg.AllowedClockSkewSeconds) * time.Second,
    HTTPTimeout: 5 * time.Second,
})
```

The interface and behavior are identical.

## Rollback Plan

If you need to rollback to the version with `identity-middleware`:

```bash
# Checkout the commit before the plugin system
git checkout <commit-before-plugin-system>

# Or use a specific version tag
git checkout v1.x.x  # version before plugin system
```

However, **rollback is not recommended** because:
- The new version maintains 100% backward compatibility
- You'll miss out on the ability to use custom plugins
- The codebase is more maintainable without the private dependency

## Frequently Asked Questions

### Q: Do I need to update my tokens?
**A:** No, all existing tokens continue to work without any changes.

### Q: Do I need to update my configuration?
**A:** No, all configuration variables remain the same.

### Q: Will this break my deployment?
**A:** No, it's a drop-in replacement with full backward compatibility.

### Q: Can I still use Tikti/Storifly authentication?
**A:** Yes! The JWKS plugin works with any JWKS-compliant auth system, including Tikti/Storifly.

### Q: What if my tokens use different claims?
**A:** The JWKS plugin supports standard JWT claims. If you have custom claims, they're available in `claims.Raw` map.

### Q: Can I use multiple auth providers?
**A:** Currently, CodeQ uses one validator for producers and one for workers. To support multiple providers, you'd need to implement a custom plugin that handles routing.

### Q: Is there a performance difference?
**A:** No significant difference. The JWKS plugin uses the same validation logic with similar caching strategies.

### Q: How do I verify the migration is successful?
**A:** Run the test suite: `go test ./...`. If all tests pass, the migration is successful.

## Getting Help

If you encounter issues during migration:

1. Check that all tests pass: `go test ./...`
2. Verify your configuration matches the examples above
3. Review the [Authentication Plugins documentation](21-authentication-plugins.md)
4. Check the [examples](../examples/custom-auth-plugin.md) for reference implementations
5. Open an issue on GitHub with details about your setup

## What's Next?

Now that you're on the plugin-based system:

1. **Explore custom plugins**: Consider implementing a custom auth plugin if you have specific needs
2. **Simplify your CI/CD**: Remove any GitHub token setup needed for private dependencies
3. **Contribute**: Share your custom plugins with the community
4. **Stay updated**: Follow the repository for new plugin examples and features

## Summary

✅ **Zero configuration changes required**  
✅ **Zero token changes required**  
✅ **100% backward compatible**  
✅ **Removes private dependency**  
✅ **Enables custom authentication methods**  
✅ **All tests passing**  

The migration is essentially automatic - just update to the latest version and you're done!
