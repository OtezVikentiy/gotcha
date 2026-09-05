# Dependencies

The "Dependencies" screen shows what your service *talks to* outside of
itself — databases, caches, and other HTTP services it calls — with call
volume, latency, and error rate for each one. It needs no separate setup: the
data comes from the same transaction/span traces already covered in
[Performance](/docs/performance).

## Where it lives

The screen opens from **"Dependencies"** in the "Performance" section
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

Above the table, a map: your service in the center, data stores (databases
and caches) in a column on the left, HTTP dependencies in a column on the
right. Each node shows the target name and a metrics line "calls · p95 ·
error rate"; a name too long for the box is truncated, the full name shows on
hover. The edge from the service to a dependency is colored by error rate
(neutral, yellow, red), so a failing dependency is visible at a glance;
hovering an edge shows the full set of metrics. Arrows on the edges show the
[data direction](#data-direction). The map shows the 16 most-called
dependencies; the rest are in the table only (a note under the map says how
many more). Below it, the same data as a table:

| Column | Meaning |
|---|---|
| Dependency | kind + target, e.g. "database: postgresql" or "http: api.stripe.com" |
| Data | data flow direction: ← reads, → writes, ⇄ both, — not determined |
| Calls | number of client-op spans in the selected window |
| p50 / p95 | duration percentiles for calls to that dependency |
| Error rate | share of calls with a status other than `ok` |

The table is capped in size (the largest dependencies by call count win); if
there are more than that, a note above the table says how many are shown.
If the project has no tracing configured yet, or no client-op spans were
recorded in the window, the screen shows an empty/error state instead of a
table — see [Performance → How to send the data](/docs/performance) for
enabling tracing in your SDK.

## Data direction

Arrows on the map and the "Data" column in the table show which way
information flows between the service and a dependency:

- **← reads** — the service receives data from the dependency (the arrow
  points at the service);
- **→ writes** — the service sends data to the dependency (the arrow points
  at the dependency);
- **⇄ both** — the window contained both reads and writes;
- **—** — no recognized operations, no arrow on the edge.

The direction is derived from the **operation verb** of each span. The span
attribute is used first — `db.operation.name` (or `db.operation`) for
databases and caches, `http.request.method` (or `http.method`) for HTTP
calls; without the attribute, the verb is the first word of the span
description (e.g. `SELECT` from `SELECT * FROM users`). Case does not matter.

Which verbs count as what:

| Kind | Read | Write |
|---|---|---|
| database | `SELECT`, `WITH`, `SHOW`, `EXPLAIN`, `DESCRIBE` | `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `REPLACE`, `UPSERT`, DDL (`CREATE`, `ALTER`, `DROP`, `TRUNCATE`), `COPY` |
| cache | `GET`, `MGET`, `HGET`, `HGETALL`, `EXISTS`, `KEYS`, `SCAN`, `LRANGE`, `SMEMBERS`, `ZRANGE` and other read commands | `SET`, `MSET`, `DEL`, `INCR`, `HSET`, `LPUSH`, `SADD`, `ZADD`, `EXPIRE`, `FLUSHDB` and other write commands |
| http | `GET`, `HEAD`, `OPTIONS` | `POST`, `PUT`, `PATCH`, `DELETE` |

A verb outside these lists (`BEGIN`, `COMMIT`, `MULTI`, non-standard commands)
counts as neither a read nor a write. If a dependency had no recognized
operation in the window, the edge has no arrow and the table shows a dash.
The read/write split is deliberately coarse — it is about the *direction of
data flow*, not the semantics of the operation: an HTTP `GET` counts as a
read even if the other side changes something on it.

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

Nor is it the [storm suppression](/docs/alert-suppression) graph: the
parent → child edges between hosts and monitors are declared by an operator
by hand on that page and have no effect on this map — or the other way
round. The limits of the dependency gate itself (which incident sources it
applies to) are described there as well.
