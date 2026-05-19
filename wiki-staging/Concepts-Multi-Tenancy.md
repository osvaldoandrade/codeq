# Multi-Tenancy

codeQ supports multiple tenants on a single server. The mechanism is key-prefix isolation: every queue index segments by a `tenantId` claim carried in the caller's JWT, so two tenants' queues live in disjoint regions of the keyspace and never appear in each other's scans. Rate limits run per tenant in an in-memory token bucket so one tenant cannot push another off the air through volume alone. The page covers how isolation is enforced, what it protects against, and what it explicitly does not.

## The tenant identity

The tenant ID comes from the JWT. Authentication validates the token and extracts a `tenantId` claim; the value is propagated through the request context and reaches the repository layer as a parameter on every queue-modifying call. The empty string is a valid tenant — it represents the legacy single-tenant case where the server was deployed without auth. In the keyspace, the empty tenant is encoded as the literal underscore `_` so the key parser can split on `/` without losing the position; see `tenantSeg` in `internal/repository/pebble/keys.go:61-66`.

Authentication is the responsibility of the [Authentication and Authorization](Concepts-Authentication-And-Authorization) layer. From the queue model's perspective, the tenant ID is a trusted opaque string by the time it arrives. The repository does not re-validate it, does not check that it is non-empty, and does not enforce any format. The server treats `acme` and `acme-corp` as entirely different tenants; case is significant; whitespace is significant. Operators are responsible for choosing a canonical form (typically a URL-safe slug) and ensuring the auth layer hands the same form to every request.

A worker carries its own JWT with its own tenant claim. The Claim path is parameterised by the worker's tenant, so workers see only their own tenant's tasks. A worker holding a token for tenant `acme` cannot claim tenant `globex` tasks even if both tenants share commands. This is enforced at the iterator level — the scan bound is `codeq/q/<cmd>/acme/...`, which contains no globex keys.

## Key-prefix isolation

The queue keys carry the tenant in the third segment:

```
codeq/q/<cmd>/<tenant>/pending/<prio_be1>/<seq_be8>/<id>
codeq/q/<cmd>/<tenant>/inprog/<id>
codeq/q/<cmd>/<tenant>/delayed/<score_be8>/<id>
codeq/q/<cmd>/<tenant>/dlq/<id>
```

The prefix `codeq/q/<cmd>/<tenant>/` is the bound for every Pebble iterator that operates on this tenant's queue. The Claim path uses `PrefixPendingPrio(cmd, tenant, prio)`; the delayed mover uses `PrefixDelayedUpTo(cmd, tenant, ...)`; the admin queue stats use `PrefixPendingAllPrios(cmd, tenant)` and the corresponding `PrefixInprog`, `PrefixDelayed`, `PrefixDLQ` bounds. None of these iterators see keys outside the tenant's prefix because Pebble's iterator API enforces the upper bound at the LSM-tree merge level — keys in other tenants are not even decoded.

Task bodies, results, lease entries, idempotency maps, and TTL index entries are not segmented by tenant in their key paths. The task body lives at `codeq/tasks/<id>` and is reachable by any code path that knows the task ID. The justification is that a task ID is itself unforgeable — a uuid v4 with 122 bits of entropy — so knowing the ID is equivalent to having been told it by the legitimate producer. The auth layer enforces that a request's claimed tenant matches the task's tenant before returning the body; see the `GetTask` path in the scheduler service. The keyspace does not enforce this on its own.

The TTL index `codeq/ttl/<expire>/<id>` similarly is not tenant-segmented. The TTL reaper walks every expired entry across tenants in score order and deletes them; this is correct because the deletion is keyed on `id`, not on tenant prefix, and the reaper's job is to free space rather than to police access.

## What multi-tenancy gives you

The isolation buys three concrete things.

The first is queue separation. Tenant A's tasks do not appear when tenant B's worker calls Claim. There is no priority inversion across tenants — a tenant A task at priority 9 will not preempt a tenant B task at priority 0 because the scan does not consider them together. Each tenant has its own per-priority FIFO order. This makes "noisy neighbour" effects on the scheduling layer impossible: tenant B's volume cannot starve tenant A's tasks because they live in different parts of the keyspace.

The second is admin scoping. Operators can run `QueueStats(cmd, tenantID)` per tenant and get counts that are scoped to that tenant only. This is useful both for capacity planning (which tenants are growing) and for billing (charge by usage). The implementation aggregates across shards within the tenant prefix; see `sharded_task_repository.go:205-218`.

The third is rate limiting. The in-memory token bucket described next runs per (scope, subject) pair, where the subject is typically the tenant ID. A tenant that bursts traffic is throttled against its own bucket, not against a global counter, so a misbehaving tenant cannot push another's traffic over a cliff.

## Per-tenant rate limiting

The in-memory limiter is in `internal/ratelimit/inmemory.go`. The structure is a map of buckets keyed by `(scope:subject)`, where each bucket carries a token count, a capacity ceiling, and a last-refill timestamp:

```go
type memBucketState struct {
    mu       sync.Mutex
    tokens   float64
    capacity float64
    lastTS   time.Time
}
```

The Allow path is standard token-bucket arithmetic: compute elapsed seconds since `lastTS`, add `elapsed * ratePerSec` tokens up to the capacity ceiling, deduct one token if available, otherwise return a `RetryAfter` hint. The arithmetic uses floating point so the bucket can refill at non-integer rates without rounding error.

There are two design choices worth flagging.

First, the limiter is per-process. There is no cross-node coordination. In a cluster of N codeQ nodes, the effective per-tenant ceiling is N times the configured `RequestsPerMinute`, because each node enforces its own bucket. The header comment in `inmemory.go:12-23` calls this out explicitly. For typical deployments this is acceptable — legitimate clients are sticky to one node within a burst, so the per-node bucket captures most of the actual rate. Operators who need strict global rate limiting need to layer a separate enforcement point in front (an API gateway or service mesh policy).

Second, the limiter is the codeQ-side enforcement of the rate config, not a passive observer. An earlier code path used a `noopLimiter` that allowed everything, which made operator-configured rate limits a silent foot-gun. The current implementation enforces whatever the configuration specifies; if the config sets a tenant's ceiling to 100 requests per minute with a burst of 20, that tenant gets 100 RPM with a 20-burst ceiling on every node.

A janitor goroutine GCs idle buckets after ten minutes of inactivity. The map stays bounded under per-subject churn even if subjects are short-lived (per-token rather than per-tenant, for example). The janitor runs once every five minutes and walks the map to find candidates; the lock hold time is short and contention with Allow is minimal.

The bucket can be reconfigured online. Each Allow call reads the current capacity from the bucket parameter and clamps the in-flight token count down to the new ceiling if it has decreased. Operators can tighten or loosen limits without restarting.

## What multi-tenancy does not give you

There is a class of guarantees the in-process model deliberately does not provide. Operators expecting these need to layer additional mechanisms or run separate codeQ instances.

There is no network-level isolation. Both tenants speak to the same gRPC server on the same TCP port. A flood of TLS handshakes from tenant A consumes the same accept queue that tenant B's connections compete for. The gRPC server is not partitioned by tenant. Mitigations exist at the transport layer (a reverse proxy that rate-limits per source IP, a network policy that restricts source CIDRs), but they live outside codeQ.

There is no separate Pebble instance per tenant. All tenants share the same set of N shards, the same WAL, the same memtables, the same SST levels. A tenant that generates a huge write spike causes compaction load that affects every tenant on the same shards. The hash partition that distributes tasks across shards is by task ID, not by tenant ID, so a single tenant's tasks spread across all shards just like any other tenant's. The choice not to partition by tenant is deliberate: it would create hotspots when one tenant dominates volume, and it would waste capacity when tenants are unevenly sized.

There is no per-tenant disk quota. codeQ does not stop accepting Enqueue from a tenant that has filled the disk. Once the disk is full, every Enqueue fails for every tenant, regardless of which one caused it. Disk quotas should be enforced at the storage layer if needed.

There is no compute isolation. Workers from any tenant compete for the same goroutine scheduler, the same CPU cores, the same memory allocator. A tenant that submits CPU-heavy artifact uploads can cause GC pressure that affects other tenants' latency. The workers themselves typically live in different processes (each tenant runs their own worker pool against codeQ), so the worker-side compute is naturally isolated, but the server-side compute is shared.

There is no separate metrics namespace. Prometheus metrics are tagged with command and status but not (by default) with tenant. An operator who wants per-tenant SLIs needs to enable the per-tenant label cardinality explicitly, weighing the metric-storage cost against the visibility benefit. See [Observability Metrics](Observability-Metrics) for the available labels.

## When to use which tenant model

Multi-tenancy in codeQ is best understood as "soft isolation suitable for trusted tenants that share an operations team". It is the right model when:

- Tenants are internal product teams within one organisation, each owning their own producers and workers but sharing the codeQ deployment.
- Workloads are similar in shape and one tenant cannot realistically saturate the cluster.
- Operators trust the auth layer to issue accurate `tenantId` claims and want the convenience of running one codeQ for many internal customers.

It is the wrong model when:

- Tenants are mutually distrusting external customers. A single misconfigured rate limit, or a single Pebble compaction spike caused by one tenant, can affect the others' latency. Hard isolation requires separate codeQ instances on separate disks.
- A tenant's data must not coexist on the same disk as another's for compliance or contractual reasons. The keys are colocated; the bytes are interleaved across SSTs. Encryption-at-rest is not tenant-specific.
- One tenant's volume routinely exceeds another's by an order of magnitude. The lack of tenant-specific shard isolation means the small tenant's tasks will be commingled with the large tenant's compaction and WAL traffic.

For these stronger isolation needs, running multiple codeQ deployments — one per tenant or one per isolation domain — is the supported answer. Each deployment is small, each has its own Pebble directory, and the operational story is simpler than trying to push the soft-isolation model past its limits.

## Operational summary

The mental model is simple: tenantId is a routing label that segments queues. Auth puts it in the context, the repository puts it in the key, and Pebble's natural iteration order keeps tenants apart. Rate limiting layers per-tenant ceilings on top so volume from one tenant cannot crowd out another. Beyond that, every other resource — Pebble state, disk, CPU, network — is shared, and a tenant in trouble is everyone's problem. Operators choosing between in-cluster multi-tenancy and separate deployments should consider the failure-mode budget: soft isolation is cheaper to run but exposes all tenants to the same operational incidents; hard isolation costs more to run but contains incidents to one tenant at a time.
