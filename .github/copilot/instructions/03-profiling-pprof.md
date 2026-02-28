# Guide 3: Profiling with pprof

## Go Built-in Profiling

**CPU Profile (30 seconds):**
```bash
# Run the server
./server

# In another terminal:
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
# In pprof shell: type "top" to see hot functions
```

**Memory Profile (heap):**
```bash
go tool pprof http://localhost:6060/debug/pprof/heap
# Commands: "top", "list functionName", "png" (generates graph)
```

**Goroutine Analysis:**
```bash
curl http://localhost:6060/debug/pprof/goroutine?debug=1 | head -50
# Shows count and stack traces; watch for goroutine leaks
```

## Codebase-Specific Hotspots

Monitor these during load testing:

1. **Task Repository** (`internal/repository/task_repository.go`)
   - Lease repair loop is a known hotspot
   - Watch for excessive lock contention during burst load
   - Profile: `go tool pprof heap` and search for redis operations

2. **Rate Limiting** (`internal/ratelimit/token_bucket.go`)
   - Per-tenant rate limiter may cause CPU spikes with many tenants
   - Check: `top` command in CPU profile shows token_bucket?

3. **Memory Plugin** (`pkg/persistence/memory/plugin.go`)
   - Default for testing; scales poorly at high concurrency
   - Use miniredis for benchmarks instead
   - Check: heap size growth during sustained load

4. **Webhook Notifier** (`internal/services/notifier_service.go`)
   - HTTP requests to external endpoints block task completion
   - I/O intensive; profile to see if significant time spent in net

## Profiling Under Load

**Combine with k6:**

Terminal 1: Start profiling server
```bash
GODEBUG=gctrace=1 ./server  # Shows GC stats
```

Terminal 2: Run load test while capturing profile
```bash
timeout 60s curl -o cpu.prof 'http://localhost:6060/debug/pprof/profile?seconds=30'
```

Terminal 3: Run k6 load test
```bash
k6 run loadtest/k6/01_sustained_throughput.js
```

After test:
```bash
go tool pprof cpu.prof
# Analyze what consumed CPU during load
```

## Memory Leak Detection

Profile heap before and after heavy load:

```bash
# Baseline
curl http://localhost:6060/debug/pprof/heap > heap_before.prof

# Run k6 for 5 minutes
k6 run --duration=5m loadtest/k6/01_sustained_throughput.js

# After load
curl http://localhost:6060/debug/pprof/heap > heap_after.prof

# Compare
go tool pprof -base=heap_before.prof heap_after.prof
# "top" shows what grew; good sign of leaks
```

## Visualizing Profiles

Convert to graphs (requires graphviz):
```bash
go tool pprof -http=:8081 cpu.prof
# Opens web UI with interactive graph
```

Export for sharing:
```bash
go tool pprof -text cpu.prof > cpu_profile.txt
go tool pprof -list=claimTask cpu.prof > function_detail.txt
```
