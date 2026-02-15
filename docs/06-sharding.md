# Sharding

## Status

**Design Phase**: Complete  
**Implementation Phase**: Not started  
**Design Document**: [Queue Sharding HLD](24-queue-sharding-hld.md)  
**Implementation Tracking**: Issue #31

## Overview

Queue sharding is designed but not yet implemented in the current codeQ service. All queues currently operate on a single KVRocks instance with logical tenant isolation but no physical distribution across multiple storage backends.

## Design Summary

The sharding design introduces horizontal scaling capability through:

- **Explicit Sharding**: Pluggable `ShardSupplier` interface that maps commands to storage backends
- **Phased Approach**: 
  - Near-term: Independent KVRocks backends per shard
  - Long-term: RAFT-based consensus storage (e.g., TiKV) for strong consistency and automatic failover
- **Tenant Isolation**: Maintained across physical shards
- **Backward Compatibility**: Single-instance deployments continue as single-shard configurations

For comprehensive design details, architecture diagrams, migration strategy, and trade-off analysis, see:

**[📋 Queue Sharding High-Level Design (HLD) and RFC](24-queue-sharding-hld.md)**

## Current Architecture

All queue operations target a single Redis-compatible backend:

- Queue keys use tenant isolation: `codeq:q:{command}:{tenantID}:{queue-type}:{priority}`
- No shard segment in key structure
- Single storage instance = single point of scale
- Vertical scaling only

## When Sharding Implementation Completes

Once implemented, the system will support:

- **Multi-shard deployments**: Distribute commands across N storage backends
- **Explicit routing control**: Operators manually assign commands to shards based on traffic patterns
- **Zero-downtime migration**: Gradual rollout from single-shard to multi-shard configurations
- **Flexible strategies**: Hash-based, range-based, or custom routing via `ShardSupplier` implementations

## Interim Scaling Strategy

Until sharding implementation completes:

1. **Vertical Scaling**: Use larger KVRocks instance types (more CPU, RAM, disk)
2. **Isolated Deployments**: Deploy separate codeQ+KVRocks pairs per region or major tenant
3. **Command Separation**: Run distinct codeQ clusters for different command namespaces
4. **Performance Tuning**: Optimize connection pools, lease durations, and Bloom filter configurations (see [Performance Tuning](17-performance-tuning.md))

## Related Documentation

- [Domain Model: ShardSupplier Interface](02-domain-model.md#shardsupplier) - Interface specification
- [Architecture](03-architecture.md) - Current single-shard architecture
- [Performance Tuning](17-performance-tuning.md) - Scaling considerations and bottleneck analysis
- [Queue Sharding HLD](24-queue-sharding-hld.md) - Complete design specification and RFC
