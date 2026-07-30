# Monitoring gotcha itself

gotcha watches your services. This page is about watching gotcha — what it
exposes about its own health, and what to look at when you suspect it is losing
data.

## The endpoints

| Path | What it is for |
|---|---|
| `/metrics` | Prometheus-format counters about buffers, drops and insert failures. Never touches the database, so it answers even when PostgreSQL or ClickHouse is down — which is exactly when you need it. |
| `/healthz` | Liveness: answers 200 while the process serves HTTP. The body still carries component state (`postgres`, `clickhouse`, `version`), but it no longer affects the status code — put your liveness probe here. |
| `/readyz` | Readiness: the same fields plus `status`, but 503 while PostgreSQL or ClickHouse is unreachable. Put readiness probes and the container healthcheck here. |
| `/version` | Build version, commit and date. |

The split matters: a liveness probe on an endpoint that fails during a storage
outage restarts a healthy process, and every restart throws away the buffers —
the very telemetry they were holding while waiting for storage to come back.

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

**`gotcha_pipeline_dropped_tasks_total{reason="…"}`** — events and transactions
the pipeline threw away. Also unrecoverable. The `reason` label tells you what to
fix:

| `reason` | What happened | What to do |
|---|---|---|
| `queue_full` | processing cannot keep up with ingest | more workers, faster PostgreSQL |
| `queue_bytes` | the queue's byte budget ran out — tasks are larger than usual | check `GOTCHA_MAX_QUEUE_BYTES` and event sizes |
| `storage_error` | the write to PostgreSQL failed (usually an upsert timeout) | fix the database; the queue is not the problem |
| `panic` | the handler crashed on one item | a product bug: send it to us with the log |
| `closed` | ingest was already stopping when the event arrived | normal during shutdown; steady growth means a restart loop |

The split matters because the first two are cured by queue size and the third is
not: no queue is large enough to make an unavailable database available.

**`gotcha_notify_pending_jobs`** / **`gotcha_notify_oldest_pending_age_seconds`** —
delivery queue depth and the age of the oldest waiting notification. The age
matters more than the depth: it is the only number that tells "the queue is empty
because everything was delivered" from "the queue is stuck". A growing age on a
live process means delivery is blocked on a channel — check
`gotcha_notify_retried_total` and the delivery log in the UI.

**`gotcha_notify_sent_total`** / **`gotcha_notify_failed_total`** /
**`gotcha_notify_retried_total`** — delivered, given up on after retries,
rescheduled. **`gotcha_notify_failed_jobs`** — how many of those given-up jobs sit
in the queue right now.

**`gotcha_memory_limit_bytes`** — the heap ceiling derived from the container's
memory limit (80% of it). Zero means there is no limit: buffers will grow until
the HOST runs out of memory, and the kernel's OOM killer gets there first — it
throws away everything buffered, not just the excess. If this reads zero, set
`mem_limit` on the container or `GOMEMLIMIT` by hand.

**`gotcha_entities_purged_total`** — rows deleted from PostgreSQL once they
outlived `GOTCHA_RETENTION_DAYS`: issues, closed incidents, regressions. This is
expected behaviour, not a failure; the counter exists because every disappearance
of data should have a number you can look at. A flat zero while retention is
configured means the purge is not running — and the issue list is showing groups
whose events are already gone.

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
   Non-zero means gotcha dropped them; the `reason` label says what to fix.
2. **Zero drops?** Then the events never arrived. Check the sender's DSN, and
   check whether the organization ran out of quota — a rejected event is counted
   under "dropped" on the organization settings page, which is a different
   counter from these. Check `/readyz` too: with PostgreSQL down, ingest still
   answers but events never reach storage.
3. **Watch the buffer while you investigate.** A flat buffer with no drops means
   ingest is healthy and the problem is upstream of gotcha.

## What's next

- [Configuration](/docs/configuration) — retention, quotas and log verbosity.
- [Backup and restore](/docs/backup-restore) — including the `.env` file.
