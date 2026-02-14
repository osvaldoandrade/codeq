# Troubleshooting Guide

This guide helps you diagnose and resolve common issues with codeQ.

## Table of Contents

- [CLI Issues](#cli-issues)
- [API Server Issues](#api-server-issues)
- [Worker Issues](#worker-issues)
- [KVRocks/Redis Issues](#kvrocksredis-issues)
- [Authentication Issues](#authentication-issues)
- [Deployment Issues](#deployment-issues)
- [Performance Issues](#performance-issues)

---

## CLI Issues

### CLI Command Not Found

**Symptoms**: `codeq: command not found`

**Solutions**:

1. **Check installation**:
   ````bash
   which codeq
   ````

2. **Install via npm**:
   ````bash
   npm install -g @osvaldoandrade/codeq
   ````

3. **Install via script**:
   ````bash
   curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
   ````

4. **Add to PATH** if installed manually:
   ````bash
   export PATH="$PATH:/path/to/codeq/directory"
   ````

### CLI Cannot Connect to API

**Symptoms**: Connection refused, timeout errors

**Solutions**:

1. **Check base URL**:
   ````bash
   codeq --base-url http://localhost:8080 task list
   ````

2. **Verify API is running**:
   ````bash
   curl http://localhost:8080/healthz
   ````

3. **Check firewall/network**:
   - Ensure no firewall blocking the port
   - Check network connectivity

4. **Set environment variable**:
   ````bash
   export CODEQ_BASE_URL=http://your-api-server:8080
   ````

### Authentication Errors

**Symptoms**: `401 Unauthorized`, `403 Forbidden`

**Solutions**:

1. **Check tokens**:
   ````bash
   codeq auth show
   ````

2. **Set tokens manually**:
   ````bash
   codeq auth set --producer-token YOUR_TOKEN
   codeq auth set --worker-token YOUR_JWT
   ````

3. **Verify token expiration** (for JWTs):
   ````bash
   # Decode JWT to check expiration
   echo "YOUR_JWT" | cut -d. -f2 | base64 -d 2>/dev/null | jq .exp
   ````

---

## API Server Issues

### Server Won't Start

**Symptoms**: Server crashes or exits immediately

**Solutions**:

1. **Check configuration file**:
   ````bash
   # Validate YAML syntax
   cat config.yaml
   ````

2. **Check KVRocks connection**:
   ````bash
   redis-cli -h localhost -p 6666 PING
   ````

3. **Review logs** for specific errors:
   ````bash
   # Set debug logging
   LOG_LEVEL=debug ./codeq-server
   ````

4. **Verify required environment variables**:
   ````bash
   echo $REDIS_ADDR
   echo $WORKER_JWKS_URL
   echo $WEBHOOK_HMAC_SECRET
   ````

### High Memory Usage

**Symptoms**: Server consumes excessive memory

**Solutions**:

1. **Check for memory leaks**:
   ````bash
   # Monitor with pprof
   curl http://localhost:8080/debug/pprof/heap > heap.prof
   go tool pprof heap.prof
   ````

2. **Limit payload sizes**: Ensure task payloads are not excessively large

3. **Review log level**: Debug logging can increase memory usage
   ````bash
   export LOG_LEVEL=info
   ````

### Slow Response Times

**Symptoms**: API requests take too long

**Solutions**:

1. **Check KVRocks latency**:
   ````bash
   redis-cli -h localhost -p 6666 --latency
   ````

2. **Review queue depths**:
   ````bash
   codeq queue inspect COMMAND_NAME
   ````

3. **Optimize worker claim settings**:
   - Reduce `waitSeconds` for long polling
   - Increase worker concurrency

4. **Enable request tracing**:
   - Check `X-Request-Id` in logs
   - Correlate slow requests

---

## Worker Issues

### Worker Not Claiming Tasks

**Symptoms**: Worker runs but never claims tasks

**Solutions**:

1. **Verify command matching**:
   ````bash
   # Worker must claim the correct command
   codeq worker start --events GENERATE_MASTER
   ````

2. **Check task availability**:
   ````bash
   codeq queue inspect GENERATE_MASTER
   ````

3. **Verify worker authentication**:
   - Ensure worker JWT has `codeq:claim` scope
   - Check `eventTypes` claim in JWT matches task commands

4. **Check lease times**:
   - Tasks already leased won't be claimed
   - Wait for lease expiration or worker heartbeat failure

### Worker Lease Expired

**Symptoms**: Tasks reassigned while worker is processing

**Solutions**:

1. **Increase lease duration**:
   ````bash
   codeq worker start --lease-seconds 600
   ````

2. **Send heartbeats**:
   - Workers must send periodic heartbeats
   - Default heartbeat interval: every 60 seconds

3. **Optimize processing time**:
   - Break long tasks into smaller chunks
   - Use async processing patterns

### NACK/Retry Loop

**Symptoms**: Tasks repeatedly fail and retry

**Solutions**:

1. **Check error logs**: Identify why tasks are failing

2. **Review backoff policy**:
   ````bash
   # Check configuration
   grep BACKOFF config.yaml
   ````

3. **Increase max attempts**:
   ````yaml
   maxAttemptsDefault: 10
   ````

4. **Fix underlying issue**: Address the root cause of failures

5. **Check DLQ**:
   ````bash
   codeq queue inspect COMMAND_NAME --dlq
   ````

---

## KVRocks/Redis Issues

### Connection Refused

**Symptoms**: Cannot connect to KVRocks

**Solutions**:

1. **Verify KVRocks is running**:
   ````bash
   redis-cli -h localhost -p 6666 PING
   ````

2. **Check configuration**:
   ````bash
   export REDIS_ADDR=localhost:6666
   ````

3. **Firewall rules**: Ensure port 6666 is accessible

4. **Authentication**:
   ````bash
   export REDIS_PASSWORD=yourpassword
   ````

### Data Inconsistency

**Symptoms**: Tasks stuck, incorrect queue counts

**Solutions**:

1. **Run admin cleanup**:
   ````bash
   codeq admin cleanup --command COMMAND_NAME
   ````

2. **Check for orphaned keys**:
   ````bash
   redis-cli -p 6666
   KEYS task:*
   KEYS queue:*
   ````

3. **Review storage layout**: See [docs/07-storage-kvrocks.md](docs/07-storage-kvrocks.md)

### High Memory Usage in KVRocks

**Symptoms**: KVRocks consuming too much memory

**Solutions**:

1. **Check key count**:
   ````bash
   redis-cli -p 6666 DBSIZE
   ````

2. **Review TTLs**: Ensure old data is expiring

3. **Run admin cleanup**: Remove completed/failed tasks

4. **Increase KVRocks memory limit** in configuration

---

## Authentication Issues

### JWT Token Expired

**Symptoms**: `401 Unauthorized` errors

**Solutions**:

1. **Refresh token**:
   ````bash
   codeq auth login
   ````

2. **Check token expiration**:
   ````bash
   # Decode JWT payload
   echo "TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | jq .
   ````

3. **Update configuration** with new token

### JWKS Validation Failure

**Symptoms**: `invalid signature`, `unable to find key`

**Solutions**:

1. **Verify JWKS URL**:
   ````bash
   curl https://your-jwks-url/.well-known/jwks.json
   ````

2. **Check issuer and audience**:
   ````yaml
   workerIssuer: https://your-issuer.com
   workerAudience: codeq-worker
   ````

3. **Ensure key ID (kid) matches**: JWT header `kid` must match JWKS key

4. **Allow clock skew**:
   ````yaml
   allowedClockSkewSeconds: 60
   ````

### Missing Scopes

**Symptoms**: `403 Forbidden` when claiming tasks

**Solutions**:

1. **Check JWT claims**:
   ````json
   {
     "scope": "codeq:claim codeq:heartbeat codeq:result",
     "eventTypes": ["GENERATE_MASTER", "GENERATE_CREATIVE"]
   }
   ````

2. **Request correct scopes** from identity provider

3. **Verify event types** match task commands

---

## Deployment Issues

### Helm Chart Installation Fails

**Symptoms**: Helm install errors

**Solutions**:

1. **Validate chart**:
   ````bash
   helm lint ./helm/codeq
   ````

2. **Check required values**:
   ````bash
   helm install codeq ./helm/codeq \
     --set secrets.webhookHmacSecret=YOUR_SECRET \
     --set config.workerJwksUrl=https://your-jwks \
     --set config.workerIssuer=https://issuer
   ````

3. **Review pod logs**:
   ````bash
   kubectl logs -l app.kubernetes.io/name=codeq
   ````

4. **Check KVRocks status** (if embedded):
   ````bash
   kubectl get pods -l app=kvrocks
   ````

### Pod CrashLoopBackOff

**Symptoms**: Pod repeatedly crashes

**Solutions**:

1. **Check logs**:
   ````bash
   kubectl logs POD_NAME --previous
   ````

2. **Verify secrets**:
   ````bash
   kubectl get secret codeq-secret -o yaml
   ````

3. **Check resource limits**: Increase if OOMKilled

4. **Verify configuration**:
   ````bash
   kubectl get configmap codeq-config -o yaml
   ````

---

## Performance Issues

### High Latency

**Symptoms**: Slow API responses

**Solutions**:

1. **Check KVRocks performance**:
   ````bash
   redis-cli -p 6666 --latency-history
   ````

2. **Optimize worker polling**:
   - Reduce `waitSeconds` to 5-10 seconds
   - Increase worker concurrency

3. **Review queue depths**: Large queues can slow operations

4. **Use connection pooling**: Ensure proper Redis connection management

### Task Delays

**Symptoms**: Tasks not processed quickly enough

**Solutions**:

1. **Increase worker count**: Scale horizontally

2. **Optimize lease times**: Balance between lease duration and processing time

3. **Check backoff settings**: Aggressive backoff can delay retries

4. **Review priority**: Higher priority tasks are processed first

---

## Getting Help

If these solutions don't resolve your issue:

1. **Search existing issues**: https://github.com/osvaldoandrade/codeq/issues
2. **Create a new issue** with:
   - Clear description of the problem
   - Steps to reproduce
   - Relevant logs and error messages
   - Environment details (OS, Go version, KVRocks version)
3. **Join discussions**: https://github.com/osvaldoandrade/codeq/discussions

## Additional Resources

- [Architecture](docs/03-architecture.md)
- [Configuration Guide](docs/14-configuration.md)
- [HTTP API Reference](docs/04-http-api.md)
- [Queue Model](docs/05-queueing-model.md)
- [Storage Layout](docs/07-storage-kvrocks.md)
