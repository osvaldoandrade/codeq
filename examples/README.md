# CodeQ Examples

This directory contains examples and guides for using and extending CodeQ.

## Authentication Examples

### [Custom Authentication Plugin](custom-auth-plugin.md)

Learn how to implement custom authentication plugins for CodeQ, including:
- Simple API key authentication
- Database-backed API keys
- OAuth2 introspection
- mTLS certificate authentication
- Best practices and real-world examples

This is useful if you want to integrate CodeQ with a custom authentication system or use an authentication method other than the default JWKS/JWT validation.

## Getting Started

1. **Using the Default JWKS Plugin**: See [Authentication Plugins Documentation](../docs/20-authentication-plugins.md)
2. **Implementing Custom Plugins**: See [Custom Auth Plugin Example](custom-auth-plugin.md)
3. **Migration Guide**: See [Plugin System Migration](../docs/21-migration-plugin-system.md)

## Contributing Examples

We welcome contributions of new examples! If you've implemented an interesting integration or have a useful pattern to share:

1. Create a markdown file in this directory
2. Include a clear explanation and code examples
3. Add a link to it in this README
4. Submit a pull request

## More Resources

- [Main Documentation](../docs/)
- [GitHub Repository](https://github.com/osvaldoandrade/codeq)
- [API Reference](../docs/18-package-reference.md)
