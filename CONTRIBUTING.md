# Contributing to codeQ

Thank you for your interest in contributing to codeQ! This guide will help you get started.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [Project Structure](#project-structure)
- [Making Changes](#making-changes)
- [Testing](#testing)
- [Documentation](#documentation)
- [Submitting Changes](#submitting-changes)

## Code of Conduct

We are committed to providing a welcoming and inclusive environment. Please be respectful and constructive in all interactions.

## Getting Started

1. **Fork the repository** on GitHub
2. **Clone your fork** locally:
   ````bash
   git clone https://github.com/YOUR_USERNAME/codeq.git
   cd codeq
   ````

3. **Add upstream remote**:
   ````bash
   git remote add upstream https://github.com/osvaldoandrade/codeq.git
   ````

## Development Setup

### Prerequisites

- **Go**: Version specified in `go.mod` (check with `go version`)
- **KVRocks**: For local testing (optional, can use Redis-compatible store)
- **Git**: For version control

### Install Dependencies

````bash
go mod download
````

### Build the CLI

````bash
go build -o codeq ./cmd/codeq
./codeq --help
````

### Run Tests

Test the CLI code (does not require private dependencies):

````bash
go test ./cmd/codeq/...
````

Test all public packages:

````bash
go test ./pkg/...
````

**Note**: Tests in `internal/` may require private server-side dependencies and are not required for CLI contributions.

## Project Structure

````
codeq/
├── cmd/codeq/          # CLI application entry point
├── pkg/                # Public Go packages
│   ├── app/           # Application wiring
│   ├── config/        # Configuration management
│   └── domain/        # Domain models
├── internal/           # Private packages (server-side)
│   ├── controllers/   # HTTP request handlers
│   ├── middleware/    # Auth, logging, etc.
│   ├── repository/    # Data access layer
│   └── services/      # Business logic
├── docs/              # Technical specification
├── wiki/              # User-facing documentation
├── helm/              # Kubernetes deployment
└── npm/               # NPM package wrapper
````

## Making Changes

### Branch Naming

Use descriptive branch names:

- `feature/add-priority-sorting`
- `fix/claim-timeout-bug`
- `docs/update-api-reference`

### Commit Messages

Follow conventional commits format:

````
<type>(<scope>): <subject>

<body>

<footer>
````

**Types**: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`

**Example**:
````
feat(cli): add --format json flag to task create

Allows JSON output for scripting and automation.

Closes #123
````

### Code Style

- Follow standard Go formatting (`gofmt`, `goimports`)
- Write clear, self-documenting code
- Add comments for complex logic
- Keep functions focused and concise

## Testing

### Unit Tests

Write unit tests for new functionality:

````bash
go test ./pkg/domain/... -v
````

### Integration Tests

Integration tests are in `pkg/app/integration_test.go`:

````bash
go test ./pkg/app/... -v
````

**Note**: Integration tests may require additional setup (KVRocks instance).

### Manual Testing

Test the CLI locally:

````bash
# Build
go build -o codeq ./cmd/codeq

# Initialize config
./codeq init

# Test against local server
./codeq --base-url http://localhost:8080 task create \
  --event TEST_EVENT \
  --payload '{"test": true}'
````

## Documentation

Documentation is critical! Please update relevant docs when making changes:

### When to Update Documentation

- **Adding a feature**: Update `docs/`, wiki pages, and README
- **Changing CLI**: Update `wiki/CLI.md` and command help text
- **API changes**: Update `docs/04-http-api.md`
- **Configuration**: Update `docs/14-configuration.md`

### Documentation Standards

Follow the **Diátaxis** framework:

1. **Tutorials**: Learning-oriented, hands-on lessons
2. **How-to guides**: Problem-oriented, practical steps
3. **Technical reference**: Information-oriented, precise descriptions
4. **Explanation**: Understanding-oriented, clarification

Use:
- Active voice
- Plain English
- Progressive disclosure (high-level first, details second)
- Code examples with expected output

### Documentation Locations

- `/docs/`: Technical specifications (architecture, API, internals)
- `/wiki/`: User-facing guides (getting started, tutorials, use cases)
- `README.md`: Project overview, quick start
- Code comments: Implementation details, rationale

## Submitting Changes

### Pull Request Process

1. **Update your branch**:
   ````bash
   git fetch upstream
   git rebase upstream/main
   ````

2. **Push to your fork**:
   ````bash
   git push origin your-branch-name
   ````

3. **Create a Pull Request** on GitHub

4. **Fill out the PR template** with:
   - Clear description of changes
   - Motivation and context
   - Testing performed
   - Documentation updates
   - Screenshots (if UI changes)

### PR Review Criteria

- Code compiles and tests pass
- Changes are focused and scoped
- Code follows style guidelines
- Documentation is updated
- No breaking changes (or clearly documented)
- Commit history is clean

### CI/CD Checks

Your PR will trigger automated checks:

- **Release workflow**: Tests CLI build
- **Static workflow**: Deploys documentation preview

Ensure all checks pass before requesting review.

## Community & Getting Help

### GitHub Discussions

Join the conversation in [GitHub Discussions](https://github.com/osvaldoandrade/codeq/discussions):

- **💡 Ideas**: Propose new features or enhancements
- **🏗️ Architecture & RFCs**: Discuss major design changes (like queue sharding, RAFT integration)
- **🙏 Q&A**: Get help with configuration, deployment, or usage questions
- **💬 General**: Share use cases, best practices, or general feedback
- **📣 Announcements**: Stay updated with releases and important changes (maintainers only)

For detailed guidance on choosing the right category, see [Discussions Guide](.github/DISCUSSIONS_GUIDE.md).

### Issues vs Discussions

**Use Issues for**:
- 🐛 Bug reports with reproducible steps
- ✨ Feature requests that are well-defined and ready for implementation
- 📝 Documentation fixes or improvements

**Use Discussions for**:
- ❓ Questions about how to use codeQ
- 💭 Brainstorming feature ideas that need refinement
- 🎯 Architecture proposals requiring community input (start here, then create formal RFC)
- 📊 Performance tuning advice
- 🌍 Use cases and success stories

### Getting Help

1. **Search first**: Check [documentation](docs/), [discussions](https://github.com/osvaldoandrade/codeq/discussions), and [issues](https://github.com/osvaldoandrade/codeq/issues)
2. **Ask in Discussions**: Use the Q&A category for questions
3. **Provide context**: Include versions, config, error messages, and what you've tried
4. **Be patient**: Community members help in their free time

Thank you for contributing! 🎉
