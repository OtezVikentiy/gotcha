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

**`gotcha_purge_queue_depth`** / **`gotcha_purge_queue_oldest_seconds`** — how
many projects are still waiting for their ClickHouse telemetry to be deleted
after the project itself was removed, and how long the oldest request has been
waiting. Deleting a project queues that work in the same transaction that
removes the row, and a background worker carries it out, so the request no
longer holds eight heavy mutations open. There are two numbers because depth
alone says nothing: one request stuck for three days looks exactly like one
queued a minute ago. A growing age means an unfulfilled obligation to delete
data — the reason for the last attempt is in `project_purge_queue.last_error`.

**`gotcha_projects_purged_total`** — projects whose telemetry has been deleted.
Like `gotcha_entities_purged_total`, it exists because every disappearance of
data should have a number.

**`gotcha_storage_free_bytes{store="…"}`** / **`gotcha_storage_total_bytes{store="…"}`**
— free and total bytes on the volume where a store physically keeps its data.
Today only `store="clickhouse"` reports them (ClickHouse's disk system table):
PostgreSQL has no way to learn the size of the underlying VOLUME over an
ordinary connection — it knows the size of its own data, not the size of the
disk under it — so these two never appear under `store="postgres"`; see
`gotcha_storage_used_bytes` below instead. The value is `NaN`, not `0`, while
the poll has never succeeded even once. **This is not "give it a couple of
minutes after startup":** the first poll is synchronous and happens right at
metric registration, before the port even opens — by the time `/metrics` is
readable at all, that first poll has already happened. So `NaN` in the output
means exactly one thing: the poll is failing — for example, the service user
lacks access to ClickHouse's disk system table, or the query to PostgreSQL is
missing its timeout. The log carries a matching entry, `storage metrics: poll
failed`, with `store` and `error` fields that say why. It retries every 5
minutes; zero wouldn't have worked here instead — it would read as "disk is
full", not as "the poll is broken".

**`gotcha_storage_used_bytes{store="postgres"}`** — how much disk space
PostgreSQL's own data currently occupies (the database size). This is **not**
free space and not the volume's total size — see the previous entry for why
PostgreSQL can't report those. To gauge how much headroom is left, compare
this number against the volume size you already know PostgreSQL runs on
(usually one volume per instance) — by hand: gotcha has no way to learn your
disk size on its own.

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

Disk space needs a separate pair of rules per store, not one for both:
ClickHouse reports a real fraction, PostgreSQL doesn't (see
`gotcha_storage_used_bytes` above), so its threshold has to be built on a
growth forecast instead of a percentage.

```
# ClickHouse: the free fraction is known directly — free and total come from
# the same system.disks row, so the ratio is honest.
gotcha_storage_free_bytes{store="clickhouse"} / gotcha_storage_total_bytes{store="clickhouse"} < 0.1

# PostgreSQL: gotcha doesn't know the volume size, so instead of a fraction
# this forecasts the trend: predict_linear extrapolates used_bytes a day
# ahead from the last 6 hours of growth. The threshold below is an example
# for a 20 GB volume (the minimum from the disk requirement), 90% of it —
# substitute 90% of YOUR known volume size in bytes.
predict_linear(gotcha_storage_used_bytes{store="postgres"}[6h], 24*3600) > 1.8e10
```

A comparison against `NaN` never passes, so while the poll has no value both
rules above stay quiet — they don't fire. That has a consequence worth
spelling out: **the rule itself won't tell you the poll is broken.** It's
built for the case where a number exists and crosses the threshold, not for
the case where there's no number at all. So a failing poll needs its own,
separate signal — not a threshold on the value, but watching for `NaN` itself
in the `/metrics` output, or more reliably, for the log entry (`storage
metrics: poll failed`, with a `store` field).

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

## When disk space is running low

1. **Gauge how full it is.** For ClickHouse, the ratio
   `gotcha_storage_free_bytes{store="clickhouse"} / gotcha_storage_total_bytes{store="clickhouse"}`:
   below 10% means you're close to trouble. PostgreSQL has no ready-made
   fraction — compare `gotcha_storage_used_bytes{store="postgres"}` against the
   volume size you already know by hand; the growth trend matters more than the
   raw number, since it tells you how much time is left, not just how much is
   used right now.
2. **Check that purging is actually running.** `gotcha_entities_purged_total`
   should climb whenever `GOTCHA_RETENTION_DAYS` is set; a flat zero means
   PostgreSQL's purge isn't working even though it should be (see above).
   ClickHouse's TTL runs automatically, but each kind of data has its own
   retention period — see [Configuration](/docs/configuration).
3. **Find what's growing fastest.** By default profiles are the heaviest per
   byte (`GOTCHA_PROFILE_RETENTION_DAYS`, which is why its default is shorter
   than the rest — 7 days). If you're sending continuous profiling but not
   regularly looking at the flamegraphs, that's the first candidate for a
   shorter retention or turning it off on the SDK side.
4. **Free space now, rather than waiting on TTL.** Deleting a project
   ("Project settings" → "Danger zone" → "Delete project") wipes its telemetry
   from ClickHouse immediately, not gradually — useful for test or abandoned
   projects that piled up data for nothing.
5. **If space is consistently tight, shorten retention.** Retention is set
   separately per kind of data (`GOTCHA_RETENTION_DAYS`,
   `GOTCHA_SPAN_RETENTION_DAYS`, `GOTCHA_METRIC_RETENTION_DAYS`,
   `GOTCHA_PROFILE_RETENTION_DAYS` — see [Configuration](/docs/configuration)).
   The change takes effect on the next start and doesn't retroactively restore
   anything already deleted; and the freed space doesn't appear instantly
   either — ClickHouse removes expired data through its normal background
   merges, not the moment you edit the config.

## What's next

- [Configuration](/docs/configuration) — retention, quotas and log verbosity.
- [Backup and restore](/docs/backup-restore) — including the `.env` file.
