# codeQ Specification

This specification defines codeq, a reactive task queue with an embedded
Pebble persistence engine (CockroachDB's RocksDB-style LSM). codeq is
implemented in Go and runs as a single binary — server, persistence
layer, lease table, and gRPC + HTTP API all in one process.

> **Note on Documentation Structure**: This `docs/` directory contains the **canonical specification and technical documentation** organized by the [Diátaxis framework](https://diataxis.fr/).

## Index

### Tutorials (Learning-Oriented)

0. `docs/00-getting-started.md` - Step-by-step tutorial for first-time users

### How-To Guides (Problem-Oriented)

13. `docs/13-examples.md` - Usage examples and common patterns
14. `docs/14-configuration.md` - Configuration reference
15. `docs/15-cli-reference.md` - Complete CLI command reference
22. `docs/22-local-development.md` - Local development with Docker Compose
26. `docs/26-load-testing.md` - Load testing framework and benchmarks
28. `docs/28-troubleshooting.md` - Troubleshooting guide for common issues
29. `docs/29-operational-runbooks.md` - Operational runbooks for incidents, maintenance, scaling, monitoring, and data management
30. `docs/30-performance-baselines.md` - Baseline load test results and regression benchmarks
33. `docs/33-staging-validation-runbook.md` - Staging performance validation runbook
34. `docs/34-streaming-api-guide.md` - gRPC streaming API tutorials, how-tos, and protocol reference

### Technical Reference (Information-Oriented)

1. `docs/01-overview.md` - System overview and goals
2. `docs/02-domain-model.md` - Core entities and relationships
3. `docs/03-architecture.md` - System architecture and components
4. `docs/04-http-api.md` - HTTP API reference
5. `docs/05-cluster-architecture.md` - Horizontal scaling via consistent-hash ring and gRPC routing
6. `docs/05-queueing-model.md` - Queue semantics and behavior
7. `docs/06-sharding.md` - Sharding strategy: Phase 8 intra-process + cluster mode
7b. `docs/07b-storage-pebble.md` - Pebble embedded storage layout
8. `docs/08b-pebble-sharding-internals.md` - Phase 8 intra-process sharding internals
9. `docs/08-consistency.md` - Consistency guarantees
10. `docs/09-security.md` - Authentication and authorization
11. `docs/10-operations.md` - Operational procedures
12. `docs/11-backoff.md` - Retry and backoff logic
13. `docs/12-webhooks.md` - Webhook notifications
17. `docs/16-workflows.md` - GitHub Actions workflows guide
18. `docs/17-performance-tuning.md` - Performance optimization and tuning guide
19. `docs/18-package-reference.md` - Package structure and codebase guide
20. `docs/19-testing.md` - Test coverage and testing strategy
21. `docs/20-authentication-plugins.md` - Authentication plugin system
22. `docs/21-developer-guide.md` - Developer guide for contributors
28. `docs/27-persistence-plugin-system.md` - Persistence plugin interface (Pebble is the supported backend; memory provider for tests)
34. `docs/34-streaming-api-guide.md` - gRPC streaming API protocol reference, throughput characteristics, and concurrency model
40. `docs/40-raft-replication.md` - Opt-in HA via hashicorp/raft: single-shard, multi-shard, failover, status endpoint, limitations

### Integration Guides

- `docs/integrations/java-integration.md` - Java SDK with Spring Boot, Quarkus, Micronaut
- `docs/integrations/nodejs-integration.md` - Node.js/TypeScript SDK with Express, NestJS, React
- `docs/integrations/python-integration.md` - Python SDK with FastAPI, Django, Flask
- `docs/integrations/go-integration.md` - Go SDK with standard library, Gin, Echo
- `sdks/README.md` - SDK overview and quick start guide
- `examples/` - Working example applications

### Deployment Assets

- `deploy/docker-compose/` - Local and single-node server Compose templates
- `deploy/kubernetes/` - Kubernetes example manifests; use Helm for server installs
- `deploy/config/codeq.example.yml` - Server configuration example (Redis backend)
- `deploy/config/codeq-pebble.example.yml` - Server configuration example (Pebble embedded backend)
- `helm/codeq/` - Helm chart and size profiles

### Explanation (Understanding-Oriented)

- `docs/40-raft-replication.md` - When to enable raft, mutual exclusion with the static-ring cluster mode, current limitations and follow-ups

### Design Documents (Architecture-Oriented)

24. `docs/24-queue-sharding-hld.md` - High-Level Design and RFC for queue sharding implementation
25. `docs/25-plugin-architecture-hld.md` - High-Level Design for plugin architecture with persistence and authentication

### Migration Guides (Task-Oriented)

31. `docs/31-persistence-migration-guide.md` - Persistence plugin migration, backward compatibility, and configuration guide
32. `docs/32-shard-migration-guide.md` - Shard migration tooling for moving tasks between shards

> Note: identity-middleware migration is covered in
> [`docs/20-authentication-plugins.md`](20-authentication-plugins.md#migration-appendix-removing-the-identity-middleware-private-dependency).
