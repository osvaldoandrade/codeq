# Contributing to codeQ

Thank you for your interest in contributing to codeQ! This guide will help you get started.

## Code of Conduct

Be respectful, inclusive, and considerate in all interactions. We aim to foster a welcoming community for everyone.

## Getting Started

### Prerequisites

- **Go**: 1.21 or later (check `go.mod` for the exact version)
- **Git**: For version control
- **KVRocks** or **Redis**: For local development and testing
- **Docker** (optional): For running KVRocks in a container

### Development Setup

See [DEVELOPMENT.md](DEVELOPMENT.md) for detailed instructions on setting up your development environment.

## How to Contribute

### Reporting Issues

Before creating an issue, please:

1. Search [existing issues](https://github.com/osvaldoandrade/codeq/issues) to avoid duplicates
2. Use the issue templates when available
3. Provide clear steps to reproduce bugs
4. Include relevant logs, error messages, and environment details

### Suggesting Features

Feature requests are welcome! Please:

1. Clearly describe the problem you're trying to solve
2. Explain your proposed solution
3. Consider backwards compatibility and existing architecture
4. Discuss significant changes in an issue before implementing

### Pull Requests

#### Before Submitting

1. **Fork** the repository and create a branch from `main`
2. **Make your changes** following the code style guidelines
3. **Add tests** for new functionality
4. **Update documentation** if you change APIs or behavior
5. **Run tests** to ensure everything passes
6. **Keep commits focused** and write clear commit messages

#### Commit Messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

````bash
feat: add support for custom backoff policies
fix: correct lease expiration calculation
docs: update API reference for webhooks
test: add integration tests for NACK flow
chore: update dependencies
````

#### PR Guidelines

- **Title**: Clear, concise description of changes
- **Description**: Explain what, why, and how
  - What problem does this solve?
  - What changes were made?
  - How can reviewers test this?
  - Any breaking changes or migration notes?
- **Small PRs**: Easier to review and merge
- **Link issues**: Reference related issues using `Fixes #123` or `Relates to #456`

#### Code Review

- Be patient and respectful
- Address feedback constructively
- Explain your reasoning if you disagree
- Mark conversations as resolved once addressed

## Development Guidelines

### Code Style

- Follow standard Go conventions and idioms
- Use `gofmt` to format code
- Keep functions small and focused
- Write clear, descriptive names
- Add comments for complex logic, not obvious code

### Testing

- Write tests for new features and bug fixes
- Aim for meaningful coverage, not just high percentages
- Use table-driven tests where appropriate
- Test edge cases and error conditions
- See [DEVELOPMENT.md](DEVELOPMENT.md) for running tests

### Documentation

Documentation should be:

- **Clear**: Easy to understand for both beginners and experts
- **Concise**: No unnecessary words
- **Accurate**: Kept in sync with code
- **Examples**: Show practical usage

Update documentation when:

- Adding or changing APIs
- Modifying configuration options
- Changing behavior
- Adding new features

Documentation locations:

- **`docs/`**: Technical specifications and design docs
- **`README.md`**: Quick start and overview
- **`wiki/`**: User guides and tutorials
- **Code comments**: For implementation details

### Architecture Principles

When contributing, keep these principles in mind:

1. **Availability over consistency**: Favor eventual consistency
2. **Pull-based workers**: No worker registry; workers claim tasks
3. **Simple storage model**: Leverage KVRocks/Redis primitives
4. **HTTP-first API**: Standard REST conventions
5. **JWT for auth**: Standard JWKS-based validation

## Project Structure

````
codeq/
├── cmd/codeq/          # CLI application
├── pkg/                # Public packages
│   ├── app/           # Application initialization
│   ├── config/        # Configuration loading
│   └── domain/        # Domain models
├── internal/          # Private implementation
│   ├── backoff/       # Retry backoff logic
│   ├── controllers/   # HTTP request handlers
│   ├── middleware/    # HTTP middleware
│   ├── providers/     # External service adapters
│   ├── repository/    # Data access layer
│   └── services/      # Business logic
├── helm/codeq/        # Helm chart for Kubernetes
├── docs/              # Technical documentation
├── wiki/              # User-facing guides
└── test/              # Integration and smoke tests
````

## Release Process

Releases are automated via GitHub Actions:

1. Version tags (`v*.*.*`) trigger the release workflow
2. Binaries are built for multiple platforms
3. GitHub Release is created with release notes
4. npm package is published to npmjs.org

See `.github/workflows/release.yml` for details.

## Getting Help

- **Issues**: For bugs and feature requests
- **Discussions**: For questions and general discussion
- **Documentation**: Start with `docs/README.md`

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
