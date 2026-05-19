# Observability: Structured Logging

Structured logging is the narrative pillar. A metric tells you the rate, a trace tells you the causal chain, a profile tells you which function is hot, and a log tells you *what this specific request was carrying when this specific thing happened*. The narrative shape is what makes logs irreplaceable: no other pillar preserves the exact arguments, the exact error message, and the exact ordering of events within a single goroutine. CodeQ commits to structured logging in JSON via Go's standard `log/slog` package, with every record carrying enough context — service name, environment, and where applicable the W3C `traceparent` — to be cross-referenced against the other pillars without any out-of-band joins.

This page documents the logger configuration in `pkg/app/application.go`, the level model controlled by `logLevel`, the output format toggle controlled by `logFormat`, the conventions for choosing between `Info`, `Warn`, `Error`, and `Debug`, the example call sites scattered through `internal/services/` and `internal/repository/`, and the practice of joining log lines to traces through the `traceparent` field.

## Why structured logging beats free-form text

A free-form log line — `"task 7f3a... failed: connection refused after 3 retries"` — is human-readable, easy to write, and operationally useless at scale. When the broker logs a million such lines a day, the only way to find anything is to grep, and the only way to count anything is to grep-count. Neither composes. You cannot ask "show me all the connection-refused failures for tenant acme on command generate_master in the last hour broken down by host" without a parser that understands the line's internal structure, and once you have a parser the line might as well have been emitted as JSON in the first place.

A structured log line emits the same information as fields:

```json
{"time":"2026-05-18T14:32:11.482Z","level":"WARN","msg":"result callback failed","service":"codeq","env":"prod","url":"https://acme.example.com/cb","command":"generate_master","tenant_id":"acme","attempt":3,"err":"connection refused","traceparent":"00-9c1b8d1e3f4a5b6c7d8e9fab1234abcd-1122334455667788-01"}
```

Each field is independently queryable. Your log aggregator can compute aggregations on `level`, `command`, `tenant_id`, and `err` without parsing the message. The `traceparent` field is what makes the line *joinable* to a trace; the `service` field is what makes it joinable to host metrics. The cost is that every log call site has to enumerate its key-value pairs, which is a discipline the codebase enforces by using slog's variadic `("key", value, "key", value, ...)` form everywhere.

## Logger setup

The logger is constructed at `pkg/app/application.go:113-129`:

```go
level := new(slog.LevelVar)
switch cfg.LogLevel {
case "debug":
    level.Set(slog.LevelDebug)
case "warn":
    level.Set(slog.LevelWarn)
case "error":
    level.Set(slog.LevelError)
default:
    level.Set(slog.LevelInfo)
}
var handler slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
if cfg.LogFormat == "text" {
    handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
}
logger := slog.New(handler).With("service", "codeq", "env", cfg.Env)
slog.SetDefault(logger)
```

Three decisions are baked into this snippet. First, the level is held in a `slog.LevelVar`, which means the level can be changed at runtime by calling `level.Set(...)` without restarting the process. CodeQ does not currently expose a runtime-change endpoint, but the indirection is there for when you want one. Second, the handler defaults to JSON; the text handler is opt-in via `logFormat: text` in `pkg/config/config.go:36` and `:484-485`. Third, every record produced through the default logger is tagged with `service=codeq` and `env=<env>`. Those two fields are the bare minimum for a multi-service log aggregator to know what the line came from. Adding them via `.With(...)` rather than at every call site is what makes them impossible to forget.

The output sink is `os.Stdout`. The deployment convention is that the container runtime (or the systemd unit) captures stdout and ships it to your log aggregator. There is no in-process log shipper, no file rotation, no buffered I/O between the application and the kernel. That is by design: log shipping is a deployment concern, not an application concern, and embedding it would couple the broker to a specific aggregator and force the broker to retain logs locally if the aggregator is down. Stdout has the right failure mode: if your collector is gone, your logs queue in the container runtime's buffer until either the collector returns or the buffer rotates.

## The level model

Four levels, with discipline about what each one means:

`debug` is for development. It includes anything an engineer might want to see when running the broker locally — every Pebble batch commit, every Raft Apply, every queue pop. Production almost never runs at debug. The cost is volume.

`info` is the operational baseline. It marks lifecycle events that an operator should see in steady state: subscription cleanups removing stale entries, reaper sweeps reporting how many tasks they processed, queue depth crossing watermarks. An example from `internal/services/subscription_cleanup_service.go:46`:

```go
s.logger.Info("subscription cleanup removed", "count", removed)
```

The convention is that the `msg` field is short, lowercase, and stable across releases — operators wire alerts and dashboards to message strings, and changing them is a breaking change. Variability goes into structured fields, never into the message.

`warn` is for recoverable problems. A webhook delivery that failed but will be retried; a reaper sweep that hit a transient error; a Raft follower that lagged but caught up. The defining property of `warn` is that the system is still healthy at the next tick. From `internal/services/result_callback_service.go:148`:

```go
s.logger.Warn("result callback failed", "url", url)
```

Note that the URL is logged but the response body is not — bodies may contain customer data, URLs are typically OK to log because they were configured by the producer. The decision about what is safe to log is per-field and worth thinking about at every call site.

`error` is for unrecoverable problems. The defining property of `error` is that someone needs to look at it. Sustained `error` lines should page; a single `error` line that recovers on its own is usually a `warn` written incorrectly. The broker uses `error` sparingly because most error conditions are retried or surfaced through metrics rather than logs.

Default is `info`. Most production deployments stay at `info`. Bumping to `debug` for a short investigation window — say, fifteen minutes during an incident — is the standard pattern; the LevelVar lets you do that without a restart if you wire up an endpoint, but the more common path today is to flip the YAML and roll the deployment.

## Joining logs to traces

The single most useful convention in the codebase is that log lines emitted from a traced code path include the `traceparent` of the current span as a structured field. This is what lets you walk from a log line to a trace mechanically. The wiring is straightforward: `tracing.TraceContextStrings(ctx)` (`internal/tracing/tracing.go:135-139`) returns the W3C strings for the current span; call sites that log within a traced operation include them as fields. In practice the codebase relies on context-carrying loggers — the `slog.Logger` is augmented with `traceparent` at the boundary of the traced operation and used throughout — so individual call sites do not have to remember the field name.

The downstream payoff is that your log aggregator's query bar becomes a trace browser. A query for `traceparent:"00-9c1b8d1e3f4a..."` returns every log line that participated in that trace, across every component that includes the field. From any one of those lines, the `traceparent` string is what you paste into Jaeger to open the visual trace. The handoff between [Observability Logging](Observability-Logging) and [Observability Tracing](Observability-Tracing) is a single copy-paste, by design.

## Example log lines

These are real shapes the broker emits, with field names taken from the source:

```json
{"time":"2026-05-18T14:00:01.234Z","level":"INFO","msg":"subscription cleanup removed","service":"codeq","env":"prod","count":12}
```

Source: `internal/services/subscription_cleanup_service.go:46`. Lifecycle, info, structured count.

```json
{"time":"2026-05-18T14:00:14.991Z","level":"WARN","msg":"subscription cleanup failed","service":"codeq","env":"prod","err":"pebble: store closed"}
```

Source: `internal/services/subscription_cleanup_service.go:42`. Recoverable (the next tick will try again), warn.

```json
{"time":"2026-05-18T14:00:22.508Z","level":"WARN","msg":"reaper tick failed","service":"codeq","env":"prod","sweep":"in_progress","err":"context deadline exceeded"}
```

Source: `internal/repository/pebble/reaper.go:148`. The `sweep` field tells you which reaper variant — `in_progress` vs `delayed` vs `dlq` — hit the error, which is the kind of structured discriminator that grep cannot produce from a free-form message.

```json
{"time":"2026-05-18T14:00:30.107Z","level":"DEBUG","msg":"reaper tick","service":"codeq","env":"prod","sweep":"delayed","processed":847}
```

Source: `internal/repository/pebble/reaper.go:152`. Debug, only visible when `logLevel: debug`.

```json
{"time":"2026-05-18T14:00:33.218Z","level":"WARN","msg":"reap requeue failed","service":"codeq","env":"prod","id":"7f3a8e2d-1b4c-4a5e-9d6f-1234abcd5678","err":"context canceled"}
```

Source: `internal/repository/pebble/reaper.go:198`. The `id` field is the task UUID, which is the join key to find the rest of that task's history.

```json
{"time":"2026-05-18T14:00:34.456Z","level":"WARN","msg":"notify failed","service":"codeq","env":"prod","err":"Post \"https://acme.example.com/notify\": dial tcp: lookup acme.example.com: i/o timeout"}
```

Source: `internal/services/notifier_service.go:174`. Error string preserved verbatim. The aggregator can extract the host from this field and chart per-host failure rates.

A practical aggregator query — written in roughly LogQL or Loki syntax — to find failed webhook deliveries to a specific tenant in the last hour:

```
{service="codeq", level="WARN"} |= "result callback failed" | json | tenant_id="acme"
```

A query to find all log lines for a specific trace:

```
{service="codeq"} | json | traceparent="00-9c1b8d1e3f4a5b6c7d8e9fab1234abcd-1122334455667788-01"
```

The second query is the one that drops you onto the narrative of a single task in seconds.

## Field naming conventions

The codebase favors short, snake_case-style structured keys: `err`, `url`, `count`, `id`, `sweep`, `command`, `tenant_id`, `traceparent`. There is no formal schema, but a few conventions hold:

- `err` for an error string, never `error` (collides with the JSON null vs. error-object debate in some aggregators).
- `id` for the task UUID; `tenant_id` for the tenant; `command` for the domain command name.
- `url` for any URL that is safe to log (configured webhook endpoints, not opaque request URIs).
- `count` and `processed` for cardinal numbers; durations as `<name>_ms` integers.
- `traceparent` for the W3C trace context string when the line participates in a trace.

Treating these as a soft schema means dashboards and saved queries written against one part of the codebase work against other parts. The discipline is shallow but the payoff compounds.

## Sensitive fields and what *not* to log

The corollary of structured logging is that you must be deliberate about what you put in a field, because every field is searchable forever. The broker does not log:

- task payloads (potentially customer data).
- idempotency keys (semi-sensitive identifiers).
- JWT tokens or any header values from inbound requests.
- response bodies from webhook receivers.
- the contents of subscription filters that may contain query strings.

What you *do* see in fields: identifiers (task UUIDs, tenant IDs, subscription IDs), categorical labels (command, status, sweep), counts and durations, and error strings from Go errors. Error strings can leak data if a downstream component formats sensitive content into its error message; the right defense is to wrap or sanitize errors at the layer that knows what is sensitive, not to filter strings at the log sink.

## Output format: JSON versus text

The default is JSON because every modern log aggregator parses it natively. The text handler exists because human reading at the terminal is sometimes easier when the line is `time=... level=INFO msg="reaper tick" sweep=delayed processed=847` rather than the JSON form. The text handler is useful in local development, in unit tests where you want greppable output, and on a debug-mode ad-hoc run; production should stay JSON. The toggle is `logFormat: text` in YAML or the equivalent env override.

A subtle point: switching formats does not change the *content* of the log lines, only the encoding. Every field that was a key in JSON is a key=value pair in text. That is what makes the two formats interchangeable — a structured log line is structured first and stringified second, never the other way around. Code that emits a log line in JSON emits the same fields in text, and vice versa. That property is what `slog`'s handler abstraction was designed for, and CodeQ uses it the way the standard library intends.

## What logs do not tell you

Logs are narrative, not quantitative. You cannot derive a histogram from log lines, even if you log every observation, because the aggregation would happen at query time on string-typed fields and would not survive sampling. For quantitative questions reach for the histograms in [Observability Metrics](Observability-Metrics). Logs also do not show causal chains across the asynchronous create-claim-complete boundary unless you wire `traceparent` through every line; the trace, documented in [Observability Tracing](Observability-Tracing), is the right tool for "what happened in what order across goroutines and processes." And logs do not show what your code was doing internally on the CPU; for that the answer is in [Observability Profiling](Observability-Profiling).

The strength of logs is that they preserve the specifics no other pillar keeps. The right place to look when you have a task ID and a question is the log aggregator filtered by that ID. From there you reach for traces, metrics, and profiles in the order that the question dictates. None of the four pillars is sufficient alone; each one is irreplaceable for the question it answers; and the discipline of including `traceparent` and the canonical structured fields on every log line is what makes the four compose into a single investigative workflow rather than four siloed data sources.
