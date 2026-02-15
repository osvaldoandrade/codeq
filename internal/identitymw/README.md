# ⚠️ DEPRECATED: Legacy Identity Middleware

**This package is deprecated and should not be used.**

## Status

This package (`internal/identitymw/`) is **legacy code** that has been superseded by the new plugin-based authentication system in `pkg/auth/`.

## Migration

**Use instead**: [`pkg/auth/jwks`](../../pkg/auth/jwks) for JWKS-based JWT validation.

The new plugin system provides:
- Cleaner interface design (`pkg/auth/Validator`)
- No private dependencies
- Better testability
- Plugin extensibility

## Why is this still here?

This code was copied from the private `github.com/codecompany/identity-middleware` package to remove the external dependency and allow anyone to build codeQ. It serves as a reference implementation but is **not used** in the current codebase.

## Removal Timeline

This package may be removed in a future release. All functionality has been migrated to `pkg/auth/`.

## Documentation

See:
- [Authentication Plugins Guide](../../docs/20-authentication-plugins.md) - Current auth system
- [Migration Guide](../../docs/23-migration-plugin-system.md) - Migration from identity-middleware
- [Plugin Architecture HLD](../../docs/25-plugin-architecture-hld.md) - Design rationale
