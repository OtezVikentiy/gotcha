# Monitoring gotcha itself

gotcha watches your services. This page is about watching gotcha — what it
exposes about its own health, and what to look at when you suspect it is losing
data.

## The endpoints

| Path | What it is for |
|---|---|
| `/metrics` | Prometheus-format counters about buffers, drops and insert failures. Never touches the database, so it answers even when PostgreSQL or ClickHouse is down — which is exactly when you need it. |
| `/healthz` | Liveness and readiness: pings both databases. Returns 503 if either is unreachable. |
| `/version` | Build version, commit and date. |

None of them require authentication and none expose personal data: `/metrics`
carries counts only, never event contents. If your instance is on the public
internet, restrict `/metrics` at the reverse proxy — the numbers reveal your
traffic volume, which you may not want to publish.

## Scraping

Any Prometheus-compatible agent works — Prometheus, VictoriaMetrics, Grafana
Agent, the OpenTelemetry Collector:

```yaml
scrape_configs:
  - job_name: gotcha
    static_configs:
      # 8080 is the in-container port; the stock compose publishes 59080
      # (GOTCHA_PORT). Use whichever port the instance is reachable on for you.
      - targets: ["gotcha.example.com:59080"]
```

## What the metrics mean

**`gotcha_writer_buffered_rows{writer="…"}`** — rows sitting in memory, waiting
to be written to ClickHouse. Writers: `events`, `spans`, `metrics`, `profiles`,
`uptime_results`. A healthy instance keeps this near zero and flushes within a
second or two. A number that climbs and stays high means ClickHouse is not
accepting writes.

**`gotcha_writer_insert_failures_total{writer="…"}`** — batch inserts that
failed. This is *not* data loss on its own: the batch goes back into the buffer
and is retried. A rising count with a stable buffer means transient errors are
being absorbed; a rising count *with* a growing buffer means the retries are not
succeeding, and you are heading for loss.

**`gotcha_writer_dropped_rows_total{writer="…"}`** — rows discarded because the
buffer hit its ceiling. **These are gone.** Any non-zero value deserves
attention; a growing one means you are losing telemetry right now.

**`gotcha_pipeline_queued_tasks`** / **`gotcha_pipeline_queue_capacity`** — depth
and size of the ingest queue that sits between the HTTP handler and the workers.
Sustained depth near capacity means the workers cannot keep up, usually because
PostgreSQL is slow (every task upserts an issue).

**`gotcha_pipeline_dropped_tasks_total`** — events and transactions the pipeline
threw away: the queue was full, or the handler panicked on one item. Also
unrecoverable.

**`gotcha_build_info`** — always 1; the version and mode are in the labels. Use
it to confirm what is actually deployed.

## Alerts worth setting

```
# Losing data right now.
increase(gotcha_writer_dropped_rows_total[5m]) > 0
increase(gotcha_pipeline_dropped_tasks_total[5m]) > 0

# Storage is not keeping up — loss is coming.
gotcha_writer_buffered_rows > 5000
gotcha_pipeline_queued_tasks / gotcha_pipeline_queue_capacity > 0.5
```

The first two are the ones to page on: they mean telemetry has already been
lost, and no retry will bring it back.

## When "some events are missing"

1. **`gotcha_writer_dropped_rows_total` and `gotcha_pipeline_dropped_tasks_total`.**
   Non-zero means gotcha dropped them, and the buffer/queue metrics say why.
2. **Zero drops?** Then the events never arrived. Check the sender's DSN, and
   check whether the organization ran out of quota — a rejected event is counted
   under "dropped" on the organization settings page, which is a different
   counter from these.
3. **Watch the buffer while you investigate.** A flat buffer with no drops means
   ingest is healthy and the problem is upstream of gotcha.

## What's next

- [Configuration](/docs/configuration) — retention, quotas and log verbosity.
- [Backup and restore](/docs/backup-restore) — including the `.env` file.
