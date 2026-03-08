# codeQ Specification

This specification defines codeQ, a reactive scheduling and completion system built on persistent queues in KVRocks. The design is inspired by Dyno Queues and its use of Dynomite as a lightweight DynamoDB-like storage layer, but codeQ targets KVRocks and is implemented in Go.

> **Note on Documentation Structure**: This `docs/` directory contains the **canonical specification and technical documentation** organized by the [Diátaxis framework](https://diataxis.fr/). The `wiki/` directory contains GitHub Pages content for the project website. When in doubt, refer to `docs/` for the authoritative documentation.

## Index

### Tutorials (Learning-Oriented)

0. `docs/00-getting-started.md` - Step-by-step tutorial for first-time users

### How-To Guides (Problem-Oriented)

13. `docs/13-examples.md` - Usage examples and common patterns
14. `docs/14-configuration.md` - Configuration reference
15. `docs/15-cli-reference.md` - Complete CLI command reference
26. `docs/26-load-testing.md` - Load testing framework and benchmarks

### Technical Reference (Information-Oriented)

1. `docs/01-overview.md` - System overview and goals
2. `docs/02-domain-model.md` - Core entities and relationships
3. `docs/03-architecture.md` - System architecture and components
4. `docs/04-http-api.md` - HTTP API reference
5. `docs/05-queueing-model.md` - Queue semantics and behavior
6. `docs/06-sharding.md` - Sharding strategy (future)
7. `docs/07-storage-kvrocks.md` - KVRocks storage layout
8. `docs/08-consistency.md` - Consistency guarantees
9. `docs/09-security.md` - Authentication and authorization
10. `docs/10-operations.md` - Operational procedures
11. `docs/11-backoff.md` - Retry and backoff logic
12. `docs/12-webhooks.md` - Webhook notifications
16. `docs/16-workflows.md` - GitHub Actions workflows guide
17. `docs/17-performance-tuning.md` - Performance optimization and tuning guide
18. `docs/18-package-reference.md` - Package structure and codebase guide
19. `docs/19-testing.md` - Test coverage and testing strategy
20. `docs/20-authentication-plugins.md` - Authentication plugin system
21. `docs/21-developer-guide.md` - Developer guide for contributors
22. `docs/22-local-development.md` - Local development with docker-compose
26. `docs/26-persistence-plugin-system.md` - Pluggable persistence backends (Redis, Memory, and extensibility)

### Integration Guides

- `docs/integrations/java-integration.md` - Java SDK with Spring Boot, Quarkus, Micronaut
- `docs/integrations/nodejs-integration.md` - Node.js/TypeScript SDK with Express, NestJS, React
- `sdks/README.md` - SDK overview and quick start guide
- `examples/` - Working example applications

### Explanation (Understanding-Oriented)

- `docs/migration.md` - General migration guide

### Design Documents (Architecture-Oriented)

24. `docs/24-queue-sharding-hld.md` - High-Level Design and RFC for queue sharding implementation
25. `docs/25-plugin-architecture-hld.md` - High-Level Design for plugin architecture with persistence and authentication

### Migration Guides (Task-Oriented)

23. `docs/23-migration-plugin-system.md` - Migration from identity-middleware to plugin system
