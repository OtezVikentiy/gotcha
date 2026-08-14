# Cardinality: why some names are grouped

If the performance or metrics page shows a warning about a limit on distinct values, and `<cardinality-limit>` appeared in the list, this page explains what happened and what to do.

## What happened

The project sent more **distinct** values of some field than the limit allows (10,000 per hour by default). Values beyond the limit are not discarded — they are grouped under a shared name, `<cardinality-limit>`.

The limited fields are the ones whose values come from your code and are open-ended by nature:

| Field | Where it comes from |
|---|---|
| Transaction name | `transaction` in Sentry SDKs, root span name in OTLP |
| Environment | `environment` in the SDK config |
| Metric name | OTLP instrument name |
| Service | `service.name` in resource attributes |
| Span operation | `op` in Sentry SDKs, `span.kind`/name in OTLP |
| Host | host name (`host.name` in OTLP resource attributes) |

## Why the limit exists at all

These values end up in ClickHouse sort keys and in the grouping of pre-aggregates. Every **new** value creates its own aggregate row — with its own percentile states — that will never merge with anything and lives out the whole retention period.

Ten endpoints means ten rows per five-minute bucket. A hundred thousand "endpoints" means a hundred thousand rows per bucket, and the storage is shared by every project on the instance. The limit protects neighbouring projects from each other more than it protects us from you.

## The real cause is almost always a variable in the name

In the overwhelming majority of cases this is neither malice nor real growth, but an identifier that slipped into a name:

```
GET /users/8812/profile     ← should be  GET /users/:id/profile
GET /orders/a7f3e9.../items ← should be  GET /orders/:id/items
queue.process.job-88213     ← should be  queue.process
environment = "web-07"      ← should be  environment = "production"
```

The examples in the warning tell you: if the values differ only by a number or a hash, you have found it.

## How to fix it

**Sentry SDKs (any language).** The transaction name is set by the framework integration from the route template. If you set it by hand, pass the template rather than the resolved path:

```python
# bad
sentry_sdk.set_transaction_name(f"GET /users/{user_id}/profile")
# good
sentry_sdk.set_transaction_name("GET /users/:id/profile")
```

If the integration itself hands you a raw path, that usually means the route is not registered with the framework (for instance, a handler mounted on a prefix). Register the route and the name becomes a template on its own.

**OpenTelemetry.** Span names are required to be low-cardinality by the specification: a path with an identifier belongs in the `http.route` or `url.path` attribute, not in the name. Check that your instrumentation does not override the name by hand.

**Environment.** This is `production`, `staging`, `dev` — a short fixed list. If a hostname, pod number or version ends up there, take it out: `release` and `server_name` exist for that.

**Metrics.** The instrument name is a constant. Everything variable belongs in attributes, not in the name.

## While you fix it

No data is lost: total throughput, durations and error counts for the project stay correct — grouped values are still counted, just under a shared name. The per-name breakdown returns as soon as the names become templates: the set of distinct values starts fresh every window.

## If you genuinely have that many values

It happens: a large monolith with thousands of real routes. Raise the limit with an environment variable:

```bash
GOTCHA_CARDINALITY_LIMIT=50000        # 0 removes the limit entirely
GOTCHA_CARDINALITY_WINDOW_SECONDS=3600
```

Mind the price when you raise it: every distinct value means separate pre-aggregate rows for every five-minute bucket — disk space and merge time in ClickHouse.

The guard itself has a bound too: it remembers values in process memory, and no more than a million of them across all projects. When that budget runs out, sets belonging to projects whose window has expired are dropped first; after that new values stop being remembered — the guard keeps working but stops growing. `gotcha_cardinality_tracked_values` in `/metrics` shows how many it holds right now. The number of distinct field NAMES per project is capped as well (200): a field name comes from the sender exactly like a value does.

## What's next

- [Performance](/docs/performance) — how to read the endpoints page.
- [Metrics](/docs/metrics) — how metric ingest works.
- [Configuration](/docs/configuration) — every environment variable.
