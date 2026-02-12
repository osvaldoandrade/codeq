# Operations

## Health

`GET /healthz` returns `{"status":"ok"}`.

## Metrics

Metrics are not implemented in the current service. Add instrumentation before relying on counters or gauges.

## Background jobs

- Subscription cleanup: removes expired webhook subscriptions on a fixed interval.

Delayed queue moves and lease expiry requeue are performed during claim operations (claim-time repair), not by a background scanner.

## Cleanup

Admin cleanup removes all structures for tasks whose retention timestamp is <= cutoff. Use `limit` to bound latency.

## Scaling

Scale horizontally. Use stateless API instances. KVRocks is the stateful component and must be scaled according to queue throughput.
