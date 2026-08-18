# Dependencies

The "Dependencies" screen shows what your service *talks to* outside of
itself — databases, caches, and other HTTP services it calls — with call
volume, latency, and error rate for each one. It needs no separate setup: the
data comes from the same transaction/span traces already covered in
[Performance](/docs/performance).

## Where it lives

The screen opens from **"Dependencies"** in the "Transactions" subsection
menu — URL `/projects/<id>/dependencies`. Filters at the top are a
[time range](/docs/time-range) (presets or a custom range); "Apply" reloads
the page with the new window.

## How nodes are derived

Every dependency is built from **client-op spans** — the spans your SDK
records when the service makes an outgoing call — filtered to three kinds:

- `db` spans (or `db.sql` / `db.query`) → grouped as **database**, labeled by
  `db.system` (e.g. `postgresql`, `mysql`) when the SDK reports it.
- other `db.*` spans (e.g. a cache client instrumented under `db.redis`) →
  grouped as **cache**, labeled by the part of the operation name after `db.`.
- `http.client` spans (or `http.client.*`) → grouped as **http**, labeled by
  the target host (`server.address`, or the domain from the request URL).

Spans are grouped by (kind, target) pair, so calling the same Postgres
instance a thousand times is one row, not a thousand. `http.server` spans
(the transaction itself, i.e. incoming requests) and internal-only spans are
never included — only outgoing calls count as dependencies.

## Reading the map and the table

Above the table, a hub-and-spoke diagram places the service in the center
with one spoke per dependency; edge color reflects error rate so a failing
dependency is visible at a glance. Below it, the same data as a sortable
table:

| Column | Meaning |
|---|---|
| Dependency | kind + target, e.g. "database: postgresql" or "http: api.stripe.com" |
| Calls | number of client-op spans in the selected window |
| p50 / p95 | duration percentiles for calls to that dependency |
| Error rate | share of calls with a status other than `ok` |

The table is capped in size (the largest dependencies by call count win); if
there are more than that, a note above the table says how many are shown.
If the project has no tracing configured yet, or no client-op spans were
recorded in the window, the screen shows an empty/error state instead of a
table — see [Performance → How to send the data](/docs/performance) for
enabling tracing in your SDK.

## What this is not

This is **not** a service-to-service topology map. A node here is an
*external* dependency — a database, a cache, or an outbound HTTP call — not
another one of your own services. If service A calls service B and both are
instrumented, this screen shows "http: b.internal" as one edge from A's point
of view; it does not stitch A's and B's traces together into a combined
graph, and it will not show B's own dependencies. Full multi-service
topology needs distributed tracing that propagates a shared trace context
across services (a single-service app, or services that don't propagate trace
headers to each other, won't produce that link).
