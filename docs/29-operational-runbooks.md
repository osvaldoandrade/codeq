# Operational Runbooks

This document provides step-by-step operational runbooks for common tasks. Each
runbook is designed so that operators can follow the procedures during incidents,
maintenance windows, or routine operations without needing deep system knowledge.

For metric definitions see [Operations](10-operations.md). For troubleshooting
specific symptoms see [Troubleshooting](28-troubleshooting.md). For performance
optimization see [Performance Tuning](17-performance-tuning.md).

---

## 1. Incident Response Runbook

### 1.1 Queue Backup Triage (Pending Queue Growing)

**When to use:** `codeq_queue_depth{queue="ready"}` exceeds your threshold or
the `ReadyQueueBacklog` alert fires.

**Diagnosis:**

```promql
# Current ready queue depth by command
max by (command) (codeq_queue_depth{queue="ready"})

# Claim rate — are workers consuming tasks?
sum by (command) (rate(codeq_task_claimed_total[5m]))

# Creation rate — is the backlog growing faster than claims?
sum by (command) (rate(codeq_task_created_total[5m]))
```

**Procedure:**

1. Check if workers are running and claiming tasks:

   ```bash
   # Verify at least one worker is polling
   curl -s http://<api-host>:8080/metrics | grep codeq_task_claimed_total
   ```

2. Compare creation rate vs claim rate. If creation exceeds claims, the
   backlog will grow:

   ```promql
   sum by (command) (rate(codeq_task_created_total[5m]))
   -
   sum by (command) (rate(codeq_task_claimed_total[5m]))
   ```

3. If workers are running but not claiming, verify the `command` value
   matches between task creation and worker poll:

   ```bash
   # Check what commands have tasks in the ready queue
   curl -s http://<api-host>:8080/metrics | grep 'codeq_queue_depth{.*queue="ready"}'
   ```

4. If workers are claiming but backlog still grows, scale workers
   horizontally (see [§3.2 Worker Scaling](#32-worker-scaling-based-on-queue-depth)).

5. If the backlog is caused by a temporary spike, verify it drains once
   the spike ends. No action needed if the steady-state claim rate
   exceeds the creation rate.

**Escalation:** If the backlog does not decrease after adding workers,
investigate KVRocks performance ([§1.4](#14-lease-expiration-spike-response))
and worker processing times.

---

### 1.2 DLQ Overflow Handling

**When to use:** `codeq_dlq_depth` exceeds your threshold or the
`DLQGrowth` alert fires.

**Diagnosis:**

```promql
# DLQ depth by command
max by (command) (codeq_dlq_depth)

# Failure rate — how fast are tasks failing?
sum by (command) (rate(codeq_task_completed_total{status="FAILED"}[5m]))

# Failure ratio
sum by (command) (rate(codeq_task_completed_total{status="FAILED"}[5m]))
/
sum by (command) (rate(codeq_task_completed_total[5m]))
```

**Procedure:**

1. **Investigate** — Identify why tasks are failing:

   ```bash
   # List failed tasks
   curl -s http://<api-host>:8080/v1/codeq/admin/tasks?status=FAILED | jq .
   ```

2. **Inspect** individual failed tasks for error details:

   ```bash
   curl -s http://<api-host>:8080/v1/codeq/tasks/<task-id> | jq '{status, resultCode, resultPayload}'
   ```

3. **Fix** the root cause (worker bug, malformed payload, downstream
   service outage).

4. **Retry** — Requeue tasks from the DLQ after the root cause is
   resolved:

   ```bash
   # Requeue a specific failed task
   curl -X POST http://<api-host>:8080/v1/codeq/tasks/<task-id>/requeue \
     -H "Authorization: Bearer <token>"
   ```

5. **Purge** — If tasks are unrecoverable (e.g., malformed payloads),
   remove them:

   ```bash
   # Purge expired/unrecoverable tasks
   curl -X POST http://<api-host>:8080/v1/codeq/admin/tasks/cleanup \
     -H "Authorization: Bearer <token>" \
     -H "Content-Type: application/json" \
     -d '{"limit": 100}'
   ```

6. **Verify** DLQ depth is decreasing:

   ```promql
   max by (command) (codeq_dlq_depth)
   ```

**Escalation:** If failure rate remains high after the fix, check for
systemic issues in downstream services or review worker error handling
logic.

---

### 1.3 Worker Starvation Diagnosis

**When to use:** Tasks are in the ready queue but `codeq_task_claimed_total`
rate is zero or near zero.

**Diagnosis:**

```promql
# Ready queue has tasks but no claims
max by (command) (codeq_queue_depth{queue="ready"}) > 0
  and on (command)
sum by (command) (rate(codeq_task_claimed_total[5m])) == 0
```

**Procedure:**

1. Verify workers are running:

   ```bash
   # Check for active worker connections (from worker host)
   curl -s http://<api-host>:8080/healthz
   ```

2. Confirm workers are polling the correct `command`:

   ```bash
   # Check which commands have ready tasks
   curl -s http://<api-host>:8080/metrics | grep 'codeq_queue_depth{.*queue="ready"}'
   ```

3. Check worker logs for connection or authentication errors:

   ```bash
   # On the worker host, check recent logs
   journalctl -u codeq-worker --since "10 minutes ago" --no-pager
   ```

4. Verify network connectivity between workers and the API:

   ```bash
   # From the worker host
   curl -sf http://<api-host>:8080/healthz && echo "OK" || echo "UNREACHABLE"
   ```

5. Check for rate limiting rejections on the worker scope:

   ```promql
   sum by (operation) (rate(codeq_rate_limit_hits_total{scope="worker"}[5m]))
   ```

6. If workers are rate-limited, increase the worker rate limit
   configuration (see [Operations § Rate Limiting](10-operations.md#rate-limiting)).

**Escalation:** If workers are running, connected, polling the correct
command, and not rate-limited, investigate KVRocks connectivity and
performance.

---

### 1.4 Lease Expiration Spike Response

**When to use:** `codeq_lease_expired_total` rate increases above baseline
or the `LeaseExpirySpiking` alert fires.

**Diagnosis:**

```promql
# Lease expiration rate
sum by (command) (rate(codeq_lease_expired_total[5m]))

# Compare with in-progress queue depth
max by (command) (codeq_queue_depth{queue="in_progress"})
```

**Procedure:**

1. Determine the affected command(s):

   ```bash
   curl -s http://<api-host>:8080/metrics | grep codeq_lease_expired_total
   ```

2. Check if workers are crashing or becoming unresponsive:

   ```bash
   # On the worker host, check for OOM kills or crashes
   dmesg | grep -i "oom\|killed" | tail -20
   journalctl -u codeq-worker --since "30 minutes ago" --no-pager | grep -i "panic\|fatal\|error"
   ```

3. If tasks take longer than the configured lease timeout, increase
   `leaseTimeout` in `config.yml`:

   ```yaml
   leaseTimeout: 120  # seconds — increase based on observed task duration
   ```

4. Restart the API to pick up the configuration change:

   ```bash
   systemctl restart codeq-api
   ```

5. Monitor lease expirations to confirm the spike subsides:

   ```promql
   sum by (command) (rate(codeq_lease_expired_total[5m]))
   ```

6. If workers are crashing due to resource exhaustion, add resource
   monitoring and consider scaling worker hosts.

**Escalation:** Persistent lease expirations despite configuration
changes indicate a deeper worker stability issue. Review worker
application logs and host-level resource metrics.

---

## 2. Maintenance Runbook

### 2.1 Rolling Upgrade with Zero Downtime

**When to use:** Deploying a new version of codeQ API.

**Prerequisites:**
- New binary or container image available
- Configuration validated against the new version
- At least 2 API instances running behind a load balancer

**Procedure:**

1. **Verify** current system health before starting:

   ```bash
   # Confirm all instances are healthy
   for host in api-1 api-2 api-3; do
     echo "$host: $(curl -sf http://$host:8080/healthz | jq -r .status)"
   done
   ```

2. **Drain** the first instance from the load balancer:

   ```bash
   # Remove instance from load balancer (example: nginx upstream)
   # Edit the upstream config to comment out api-1, then reload
   nginx -s reload
   ```

3. **Wait** for in-flight requests to complete (30-60 seconds):

   ```bash
   sleep 60
   ```

4. **Upgrade** the instance:

   ```bash
   # Stop the service
   systemctl stop codeq-api

   # Replace the binary (or pull new image)
   cp /path/to/new/codeq /usr/local/bin/codeq

   # Start the service
   systemctl start codeq-api
   ```

5. **Verify** the upgraded instance is healthy:

   ```bash
   curl -sf http://api-1:8080/healthz | jq .
   ```

6. **Re-add** the instance to the load balancer and reload:

   ```bash
   nginx -s reload
   ```

7. **Monitor** for errors after re-adding:

   ```promql
   # Watch for elevated error rates
   sum by (command, status) (rate(codeq_task_completed_total{status="FAILED"}[1m]))
   ```

8. **Repeat** steps 2-7 for each remaining instance.

9. **Verify** full cluster health after all instances upgraded:

   ```bash
   for host in api-1 api-2 api-3; do
     echo "$host: $(curl -sf http://$host:8080/healthz | jq -r .status)"
   done
   ```

**Rollback:** If the upgraded instance fails health checks, stop it,
restore the previous binary, and start it again. Do not proceed with
other instances until the issue is resolved.

---

### 2.2 KVRocks / Redis Maintenance Window

**When to use:** Performing KVRocks upgrades, configuration changes, or
host maintenance.

**Prerequisites:**
- Maintenance window scheduled and communicated
- If using replication, a replica is available for failover

**Procedure:**

1. **Announce** the maintenance window to stakeholders.

2. **Check** current KVRocks status:

   ```bash
   redis-cli -h <kvrocks-host> -p 6379 INFO server
   redis-cli -h <kvrocks-host> -p 6379 INFO memory
   redis-cli -h <kvrocks-host> -p 6379 INFO clients
   ```

3. **Pause** task creation if possible (reduce load during maintenance):

   ```bash
   # Optional: temporarily block producers via rate limiting
   # Set producer requestsPerMinute to a very low value
   ```

4. **Perform** the maintenance (upgrade, config change, etc.):

   ```bash
   # Example: KVRocks restart
   systemctl stop kvrocks
   # Perform maintenance (upgrade binary, edit config, etc.)
   systemctl start kvrocks
   ```

5. **Verify** KVRocks is accepting connections:

   ```bash
   redis-cli -h <kvrocks-host> -p 6379 ping
   # Expected: PONG
   ```

6. **Verify** codeQ API can connect:

   ```bash
   curl -sf http://<api-host>:8080/healthz | jq .
   ```

7. **Resume** normal operations and confirm metrics are flowing:

   ```promql
   up{job="codeq"}
   ```

**Rollback:** If KVRocks fails to start after the change, restore the
previous binary or configuration from backup and restart.

---

### 2.3 Certificate Rotation for JWT / JWKS

**When to use:** JWT signing certificates are expiring or compromised and
need rotation.

**Prerequisites:**
- New signing key generated
- Updated JWKS endpoint or key file available
- See [Security](09-security.md) for authentication configuration

**Procedure:**

1. **Generate** a new signing key (if not already done):

   ```bash
   # Example: generate RSA key pair
   openssl genrsa -out new-signing-key.pem 2048
   openssl rsa -in new-signing-key.pem -pubout -o new-public-key.pem
   ```

2. **Update** the JWKS endpoint or key file to include both the old and
   new keys (overlap period):

   ```bash
   # Add the new public key to your JWKS endpoint or key file
   # Both old and new keys should be valid during the transition
   ```

3. **Update** codeQ configuration to reference the new key:

   ```yaml
   # config.yml
   auth:
     jwksUrl: "https://auth.example.com/.well-known/jwks.json"
     # Or for file-based keys:
     # jwtPublicKeyFile: "/etc/codeq/new-public-key.pem"
   ```

4. **Rolling restart** the API instances to pick up the new configuration
   (follow [§2.1 Rolling Upgrade](#21-rolling-upgrade-with-zero-downtime)).

5. **Verify** authentication still works:

   ```bash
   # Test with a token signed by the new key
   curl -sf -H "Authorization: Bearer <new-token>" \
     http://<api-host>:8080/v1/codeq/tasks?command=test | jq .
   ```

6. **Remove** the old key from the JWKS endpoint after all tokens signed
   with the old key have expired.

7. **Confirm** no authentication failures in logs or metrics.

---

### 2.4 Configuration Changes with Validation

**When to use:** Modifying `config.yml` in a running environment.

**Procedure:**

1. **Backup** the current configuration:

   ```bash
   cp /etc/codeq/config.yml /etc/codeq/config.yml.bak.$(date +%Y%m%d%H%M%S)
   ```

2. **Edit** the configuration file:

   ```bash
   vi /etc/codeq/config.yml
   ```

3. **Validate** YAML syntax:

   ```bash
   python3 -c "import yaml; yaml.safe_load(open('/etc/codeq/config.yml'))" && echo "Valid YAML"
   ```

4. **Apply** the configuration with a rolling restart
   (follow [§2.1 Rolling Upgrade](#21-rolling-upgrade-with-zero-downtime)).

5. **Verify** the API starts correctly with the new configuration:

   ```bash
   curl -sf http://<api-host>:8080/healthz | jq .
   ```

6. **Monitor** for any adverse effects:

   ```promql
   # Check for elevated error rates after config change
   sum(rate(codeq_task_completed_total{status="FAILED"}[5m]))
   ```

**Rollback:** Restore the backup and restart:

```bash
cp /etc/codeq/config.yml.bak.<timestamp> /etc/codeq/config.yml
systemctl restart codeq-api
```

---

## 3. Scaling Runbook

### 3.1 Horizontal Scaling (Add / Remove API Instances)

**When to use:** API response latency is high or you need to handle more
concurrent requests.

**Prerequisites:**
- Stateless API instances (all state is in KVRocks)
- Load balancer in front of API instances

**Adding an instance:**

1. **Deploy** a new codeQ instance with the same configuration:

   ```bash
   # On the new host — copy config from an existing instance
   scp <existing-host>:/etc/codeq/config.yml /etc/codeq/config.yml
   systemctl start codeq-api
   ```

2. **Verify** the new instance is healthy:

   ```bash
   curl -sf http://<new-host>:8080/healthz | jq .
   ```

3. **Add** the instance to the load balancer:

   ```bash
   # Add new-host to the upstream config, then reload
   nginx -s reload
   ```

4. **Verify** traffic is flowing to the new instance:

   ```promql
   # Check that the new instance is being scraped
   up{instance="<new-host>:8080"}
   ```

**Removing an instance:**

1. **Drain** the instance from the load balancer:

   ```bash
   # Remove host from upstream config, then reload
   nginx -s reload
   ```

2. **Wait** for in-flight requests to complete:

   ```bash
   sleep 60
   ```

3. **Stop** the instance:

   ```bash
   systemctl stop codeq-api
   ```

4. **Verify** remaining instances are healthy and handling the load:

   ```promql
   # Confirm no increase in error rate
   sum(rate(codeq_task_completed_total{status="FAILED"}[5m]))
   ```

---

### 3.2 Worker Scaling Based on Queue Depth

**When to use:** Ready queue depth is consistently above your target
threshold.

**Decision metrics:**

```promql
# Current ready queue depth
max by (command) (codeq_queue_depth{queue="ready"})

# Current claim throughput
sum by (command) (rate(codeq_task_claimed_total[5m]))

# Average task processing time
sum by (command) (rate(codeq_task_processing_latency_seconds_sum{status="COMPLETED"}[5m]))
/
sum by (command) (rate(codeq_task_processing_latency_seconds_count{status="COMPLETED"}[5m]))
```

**Scaling guidelines:**

| Queue depth | Action |
|---|---|
| < 100 | Normal operations, no action |
| 100-1000 | Monitor trend; scale if sustained >10 min |
| 1000-10000 | Add 2-4 worker instances |
| > 10000 | Add 5+ worker instances; investigate root cause |

**Procedure:**

1. **Calculate** the required number of workers:

   ```
   required_workers = creation_rate / (1 / avg_processing_time)
   ```

2. **Deploy** additional worker instances:

   ```bash
   # On each new worker host
   systemctl start codeq-worker
   ```

3. **Verify** workers are claiming tasks:

   ```promql
   sum by (command) (rate(codeq_task_claimed_total[1m]))
   ```

4. **Monitor** queue depth to confirm it is decreasing:

   ```promql
   max by (command) (codeq_queue_depth{queue="ready"})
   ```

5. **Scale down** when queue depth stabilizes below threshold. Stop
   excess workers gracefully:

   ```bash
   systemctl stop codeq-worker
   ```

---

### 3.3 KVRocks Scaling and Replication

**When to use:** KVRocks is under memory or CPU pressure, or you need
high availability.

**Diagnosis:**

```bash
# Check memory usage
redis-cli -h <kvrocks-host> -p 6379 INFO memory

# Check connected clients
redis-cli -h <kvrocks-host> -p 6379 INFO clients

# Check command latency
redis-cli -h <kvrocks-host> -p 6379 INFO stats
```

**Setting up replication:**

1. **Deploy** a new KVRocks instance as a replica:

   ```bash
   # On the replica host, start KVRocks with replication config
   # In kvrocks.conf:
   # slaveof <primary-host> 6379
   systemctl start kvrocks
   ```

2. **Verify** replication is established:

   ```bash
   redis-cli -h <replica-host> -p 6379 INFO replication
   # Expect: role:slave, master_link_status:up
   ```

3. **Configure** read replicas in the load balancer if supported by your
   deployment.

**Vertical scaling:**

1. **Schedule** a maintenance window (see [§2.2](#22-kvrocks--redis-maintenance-window)).

2. **Stop** KVRocks:

   ```bash
   systemctl stop kvrocks
   ```

3. **Update** resource limits (memory, CPU) in the host or container
   configuration.

4. **Start** KVRocks and verify:

   ```bash
   systemctl start kvrocks
   redis-cli -h <kvrocks-host> -p 6379 ping
   ```

---

## 4. Monitoring & Alerting Runbook

### 4.1 Prometheus Alert Response Procedures

The alerting rules are defined in
`deploy/docker-compose/local-dev/alerting-rules.yml`. Below are
response procedures for each alert.

| Alert | Severity | Response |
|---|---|---|
| `DLQGrowthWarning` | warning | Investigate DLQ ([§1.2](#12-dlq-overflow-handling)) |
| `DLQGrowthCritical` | critical | Immediately investigate DLQ ([§1.2](#12-dlq-overflow-handling)) |
| `ReadyQueueBacklog` | warning | Triage queue backup ([§1.1](#11-queue-backup-triage-pending-queue-growing)) |
| `HighTaskFailureRate` | critical | Check worker logs, review failed tasks |
| `SlowTaskProcessing` | warning | Review [Performance Tuning](17-performance-tuning.md) |
| `LeaseExpirySpiking` | warning | Follow lease expiration procedure ([§1.4](#14-lease-expiration-spike-response)) |
| `NoClaimsDespiteReadyQueue` | critical | Diagnose worker starvation ([§1.3](#13-worker-starvation-diagnosis)) |
| `WebhookFailureRate` | warning | Check subscriber endpoints ([Webhooks](12-webhooks.md)) |
| `SustainedRateLimiting` | warning | Review rate limit config ([Operations](10-operations.md#rate-limiting)) |
| `TargetDown` | critical | Verify instance is running, check network |

**General alert response workflow:**

1. **Acknowledge** the alert in your alerting system.
2. **Diagnose** using the linked runbook section.
3. **Act** — follow the procedure to resolve.
4. **Verify** metrics return to normal.
5. **Document** the incident and any follow-up actions.

---

### 4.2 Grafana Dashboard Interpretation

Import the example dashboard from `docs/grafana/codeq-dashboard.json`.

**Key panels and what they show:**

| Panel | What to look for |
|---|---|
| Queue depth | Sustained growth indicates workers cannot keep up |
| Task throughput | Creation rate should roughly match claim + completion rates |
| DLQ depth | Any growth requires investigation |
| Processing latency (p95) | Spikes may indicate KVRocks slowness or worker issues |
| Webhook delivery success rate | Below 100% means subscribers are failing |
| Rate limit hits | Sustained hits may need config adjustment |

**Reading the dashboard during an incident:**

1. Open the Grafana dashboard and set the time range to cover the incident
   window.
2. Check the **Queue depth** panel first — this shows overall system health.
3. Look at **Task throughput** to understand if the issue is with creation,
   claiming, or completion.
4. Check **Processing latency** to identify if tasks are taking longer than
   expected.
5. Review **DLQ depth** for task failure patterns.

---

### 4.3 Key Metrics and Their Significance

| Metric | Significance | Healthy range |
|---|---|---|
| `codeq_queue_depth{queue="ready"}` | Tasks waiting for workers | < 100 (steady state) |
| `codeq_queue_depth{queue="in_progress"}` | Tasks being processed | Proportional to worker count |
| `codeq_dlq_depth` | Failed tasks requiring attention | 0 (ideal) |
| `codeq_task_claimed_total` rate | Worker throughput | Matches creation rate |
| `codeq_task_completed_total` rate | Task completion throughput | Matches claim rate |
| `codeq_lease_expired_total` rate | Workers failing mid-task | Near 0 |
| `codeq_webhook_deliveries_total{outcome="failure"}` rate | Webhook delivery failures | Near 0 |
| `codeq_rate_limit_hits_total` rate | Rate limit rejections | Near 0 (normal) |

**Health check PromQL queries:**

```promql
# System health snapshot — run these periodically
max by (command, queue) (codeq_queue_depth)
max by (command) (codeq_dlq_depth)
sum by (command) (rate(codeq_task_claimed_total[5m]))
sum by (command, status) (rate(codeq_task_completed_total[5m]))
sum by (command) (rate(codeq_lease_expired_total[5m]))
```

---

### 4.4 Escalation Procedures

| Level | Criteria | Action |
|---|---|---|
| L1 — On-call | Any warning alert | Follow runbook, resolve if possible |
| L2 — Engineering | Critical alert or L1 unable to resolve in 30 min | Page engineering team |
| L3 — Architecture | Systemic issue (multiple alerts, data loss risk) | Engage architecture team |

**Escalation checklist:**

- [ ] Alert acknowledged and time-stamped
- [ ] Initial diagnosis performed using relevant runbook section
- [ ] Current metric values captured (screenshot or PromQL results)
- [ ] Affected commands/queues identified
- [ ] Impact assessment (number of affected tasks, duration)
- [ ] Escalation communicated with context to the next level

---

## 5. Data Management Runbook

### 5.1 DLQ Inspection and Retry

**When to use:** Tasks are in the DLQ and need to be reviewed and
potentially retried.

**Procedure:**

1. **List** failed tasks:

   ```bash
   curl -s http://<api-host>:8080/v1/codeq/admin/tasks?status=FAILED | jq '.[] | {id, command, resultCode}'
   ```

2. **Inspect** a specific task for error details:

   ```bash
   curl -s http://<api-host>:8080/v1/codeq/tasks/<task-id> | jq .
   ```

3. **Decide** whether to retry or discard:
   - Retry if the root cause is fixed (transient error, downstream
     service restored).
   - Discard if the task payload is malformed or the task is no longer
     relevant.

4. **Retry** a task:

   ```bash
   curl -X POST http://<api-host>:8080/v1/codeq/tasks/<task-id>/requeue \
     -H "Authorization: Bearer <token>"
   ```

5. **Verify** the retried task progresses:

   ```bash
   curl -s http://<api-host>:8080/v1/codeq/tasks/<task-id> | jq .status
   ```

---

### 5.2 Task Purge Operations

**When to use:** Cleaning up old completed or failed tasks to reclaim
storage.

**Procedure:**

1. **Check** current task counts:

   ```promql
   max by (command, queue) (codeq_queue_depth)
   ```

2. **Run** cleanup with a bounded limit to control latency:

   ```bash
   curl -X POST http://<api-host>:8080/v1/codeq/admin/tasks/cleanup \
     -H "Authorization: Bearer <token>" \
     -H "Content-Type: application/json" \
     -d '{"limit": 500}'
   ```

3. **Repeat** as needed until the desired number of tasks are purged:

   ```bash
   # Loop cleanup in batches
   for i in $(seq 1 10); do
     curl -X POST http://<api-host>:8080/v1/codeq/admin/tasks/cleanup \
       -H "Authorization: Bearer <token>" \
       -H "Content-Type: application/json" \
       -d '{"limit": 500}'
     sleep 2  # Brief pause between batches
   done
   ```

4. **Verify** storage was reclaimed:

   ```bash
   redis-cli -h <kvrocks-host> -p 6379 INFO memory
   ```

---

### 5.3 Backup and Restore

**When to use:** Before major maintenance or for disaster recovery
preparation.

**Backup procedure:**

1. **Create** a KVRocks snapshot:

   ```bash
   redis-cli -h <kvrocks-host> -p 6379 BGSAVE
   ```

2. **Wait** for the snapshot to complete:

   ```bash
   redis-cli -h <kvrocks-host> -p 6379 LASTSAVE
   # Repeat until timestamp updates
   ```

3. **Copy** the snapshot to backup storage:

   ```bash
   # KVRocks data directory (check kvrocks.conf for exact path)
   cp -r /var/lib/kvrocks/data /backup/kvrocks-$(date +%Y%m%d%H%M%S)/
   ```

4. **Backup** the codeQ configuration:

   ```bash
   cp /etc/codeq/config.yml /backup/config-$(date +%Y%m%d%H%M%S).yml
   ```

**Restore procedure:**

1. **Stop** codeQ API instances:

   ```bash
   systemctl stop codeq-api
   ```

2. **Stop** KVRocks:

   ```bash
   systemctl stop kvrocks
   ```

3. **Restore** data from backup:

   ```bash
   cp -r /backup/kvrocks-<timestamp>/data /var/lib/kvrocks/data
   ```

4. **Start** KVRocks and verify:

   ```bash
   systemctl start kvrocks
   redis-cli -h <kvrocks-host> -p 6379 ping
   ```

5. **Restore** codeQ configuration if needed:

   ```bash
   cp /backup/config-<timestamp>.yml /etc/codeq/config.yml
   ```

6. **Start** codeQ API instances:

   ```bash
   systemctl start codeq-api
   ```

7. **Verify** system health:

   ```bash
   curl -sf http://<api-host>:8080/healthz | jq .
   ```

---

### 5.4 Data Migration Between Environments

**When to use:** Migrating task data from one environment to another
(e.g., staging to production, or between KVRocks instances).

**Procedure:**

1. **Export** tasks from the source environment:

   ```bash
   # List tasks to export
   curl -s http://<source-host>:8080/v1/codeq/admin/tasks?status=COMPLETED \
     | jq . > tasks-export.json
   ```

2. **Validate** the exported data:

   ```bash
   jq 'length' tasks-export.json
   # Verify expected count
   ```

3. **Import** tasks into the target environment:

   ```bash
   # Recreate tasks in the target environment
   cat tasks-export.json | jq -c '.[]' | while read -r task; do
     curl -X POST http://<target-host>:8080/v1/codeq/tasks \
       -H "Authorization: Bearer <token>" \
       -H "Content-Type: application/json" \
       -d "$task"
   done
   ```

4. **Verify** task counts in the target environment:

   ```bash
   curl -s http://<target-host>:8080/metrics | grep codeq_queue_depth
   ```

**Note:** For large-scale data migration, consider using KVRocks
replication instead of the API-based approach above. See
[§3.3 KVRocks Scaling](#33-kvrocks-scaling-and-replication) for
replication setup.
