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
| `/version` | Build metadata: `version`, `commit`, `date`, `go` (the Go runtime the binary was built with) and `stamped` — whether git metadata was baked into the build. `stamped: false` means the image was built outside `make` and the version is the source default, not a verified release. |

The split matters: a liveness probe on an endpoint that fails during a storage
outage restarts a healthy process, and every restart throws away the buffers —
the very telemetry they were holding while waiting for storage to come back.

None of them require authentication and none expose personal data: `/metrics`
carries counts only, never event contents. If your instance is on the public
internet, restrict `/metrics` at the reverse proxy — the numbers reveal your
traffic volume, which you may not want to publish.

## When the container goes unhealthy

The stock compose file gives the `gotcha` container a healthcheck on `/readyz`.
The probe is a subcommand of the binary itself — `gotcha --healthcheck` — so it
survives a move to a distroless base where no curl exists. Its target URL is
built from `GOTCHA_ADDR` (host always `127.0.0.1`: the probe talks to itself),
so changing the listen port does not leave the probe knocking on a dead
`:8080`; for non-standard setups — TLS termination inside the container, a
different path — override it with `--healthcheck-url=<url>`.

But be clear about what the healthcheck buys: **`docker compose` does not restart an
unhealthy container.** A failed healthcheck only changes the label in
`docker ps` (reacting to unhealthy is a Swarm/Kubernetes feature); the
`restart` policy catches a crashed process, not a hung one. Check the state
and the last probe outputs with:

```bash
docker ps                                                # STATUS column: (healthy) / (unhealthy)
docker inspect --format '{{json .State.Health}}' gotcha-gotcha-1
```

To make that state visible from the outside, don't watch the label — watch the
service: point an uptime monitor at `/readyz` from another gotcha instance
(uptime monitoring of an HTTP endpoint is exactly what the product does), or
alert on the `gotcha_up` metric of your scraper.

There is deliberately no auto-healer watching the Docker socket in the stock
setup: access to the socket is root on the host, and shipping that would trade
the security of the whole install for a scenario a supervisor solves better.

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
`logs`, `uptime_results`. A healthy instance keeps this near zero and flushes
within a second or two. A number that climbs and stays high means ClickHouse
is not accepting writes.

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

**`gotcha_pipeline_queued_bytes`** — bytes held by tasks waiting in that queue.
The queue has a byte budget as well as a task count (`GOTCHA_MAX_QUEUE_BYTES`):
a thousand small events and a thousand megabyte-sized ones are very different
loads at the same depth. When drops show `reason="queue_bytes"`, this is the
budget that ran out.

**`gotcha_ingest_key_rejections_total{reason="…"}`** — requests rejected during
key authentication, before quotas and before the body is even parsed. None of
this ever reached a project, so it's invisible in that project's own drop
counters — this is the only place it shows up at all. The `reason` label
separates client-side mistakes from each other:

| `reason` | What happened |
|---|---|
| `missing_key` | a Sentry-style request arrived with no `sentry_key` at all |
| `invalid_key` | `sentry_key` was sent but doesn't resolve to any project (typo, revoked key) |
| `project_mismatch` | the key resolves fine, but to a project other than the one in the request path — usually a DSN copied into the wrong project |
| `missing_bearer` | an OTLP request arrived with no `Authorization: Bearer` header |
| `invalid_dsn_key` | the OTLP bearer token doesn't resolve to any DSN |

A steady trickle is normal — scanners and stale SDK configs hit this. A step
change after a deploy usually means a DSN or project ID changed and one
sender wasn't updated.

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

**`gotcha_cardinality_collapsed_total`** / **`gotcha_cardinality_tracked_values`**
— the cardinality guard at work: how many open-field values (transaction names,
environments, metric names, services, operations) were collapsed into the
overflow bucket because a project hit `GOTCHA_CARDINALITY_LIMIT`, and how many
distinct values the guard is remembering right now. A growing collapsed counter
means part of the names have disappeared from lists — the affected screens show
a notice; the usual cause is an identifier that leaked into a name. The tracked
gauge is the guard's own memory footprint.

**`gotcha_host_evaluator_last_tick_timestamp_seconds`** /
**`gotcha_host_evaluator_tick_duration_seconds`** — when the host evaluator (disk,
memory, load, silence) last completed a pass, and how long that pass took. What
needs watching here is not failure but continuation: a dead evaluator looks
exactly like "all hosts are fine", because silence is its normal output. A gap
between now and the timestamp noticeably larger than `GOTCHA_HOST_EVAL_INTERVAL`
means host thresholds are not being evaluated; a duration approaching the
interval means the evaluator is falling behind — usually a slow ClickHouse or a
fleet that outgrew the interval.

Only a pass that ran to completion moves the timestamp. A pass cut short by its
own deadline (it evaluates part of the fleet and gives up) does NOT refresh it —
otherwise an evaluator that gives up halfway through every single time would
look perfectly healthy from here. The duration is always published: that's what
makes hitting the budget visible. The reason shows up in the log as `tick did
not finish within its budget`.

**`gotcha_slo_evaluator_last_tick_timestamp_seconds`** /
**`gotcha_slo_evaluator_tick_duration_seconds`** — when the SLO burn-rate
evaluator last completed a pass over every enabled SLO, and how long it took.
Same blind spot as the host evaluator: silence is the normal output, so a dead
evaluator looks exactly like "every error budget is fine". A gap between now and
the timestamp noticeably larger than `GOTCHA_SLO_EVAL_INTERVAL` means burn rates
are not being recomputed and error-budget incidents are neither opened nor
closed; a duration approaching the interval means the evaluator is falling
behind.

**`gotcha_trace_evaluator_last_tick_timestamp_seconds`** /
**`gotcha_trace_evaluator_tick_duration_seconds`** — when the performance
regression evaluator last completed a pass over every project, and how long it
took. Same blind spot as the host evaluator above: silence is the normal
output, so a dead evaluator looks exactly like "no regressions right now". It
runs every 5 minutes (fixed, no config knob); a gap noticeably larger than
that means regression alerts have stopped firing, and a duration approaching
the interval means ClickHouse is falling behind.

**`gotcha_metric_evaluator_last_tick_timestamp_seconds`** /
**`gotcha_metric_evaluator_tick_duration_seconds`** — when the metric
threshold evaluator last completed a pass over every rule, and how long it
took. Same blind spot again. A gap noticeably larger than
`GOTCHA_METRIC_EVAL_INTERVAL` (default 60s) means metric-rule alerts are not
being evaluated; a duration approaching the interval means the evaluator is
falling behind.

**`gotcha_profile_evaluator_last_tick_timestamp_seconds`** /
**`gotcha_profile_evaluator_tick_duration_seconds`** — when the profile
regression evaluator last completed a pass over every service, and how long
it took. A gap noticeably larger than `GOTCHA_PROFILE_EVAL_INTERVAL` (default
300s) means profile regression alerts are not being evaluated; a duration
approaching the interval means ClickHouse is falling behind.

**`gotcha_escalation_scheduler_last_tick_timestamp_seconds`** /
**`gotcha_escalation_scheduler_tick_duration_seconds`** — when the
centralized escalation scheduler last completed a pass over all six incident
sources (performance regressions, metric rules, profile regressions, host
thresholds, SLO burn rate, uptime), and how long it took. A dead scheduler
looks exactly like "nothing needs escalating" — every ladder simply stops
advancing. A gap noticeably larger than `GOTCHA_ESCALATION_INTERVAL` (default
60s) means escalation steps and reminders are not firing for any source; a
duration approaching the interval means PostgreSQL is not keeping up. A tick
that runs out of budget partway through skips the remaining bindings for that
pass rather than blocking the next one — check the log for `escalation
scheduler: tick did not finish within its budget`.

**`gotcha_uptime_scheduler_last_tick_timestamp_seconds`** /
**`gotcha_uptime_scheduler_tick_duration_seconds`** — when the uptime check
scheduler last completed a pass enqueuing due checks, and how long it took. A
stale timestamp means monitors show as enabled but nothing is actually being
checked — state freezes at whatever it last was, and missed heartbeats or
expiring certificates go uncounted. This runs in every process with an uptime
service, not just `--mode=uptime`; on `--mode=web` it logs a warning that
checks are scheduled but not executed there.

**`gotcha_uptime_runner_last_tick_timestamp_seconds`** /
**`gotcha_uptime_runner_tick_duration_seconds`** — when the uptime runner
(`--mode=uptime`/`all`) last completed a pass leasing and dispatching due
checks, and how long it took. A stale timestamp means checks are enqueued by
the scheduler above but nobody is executing them.

**`gotcha_uptime_watchdog_last_tick_timestamp_seconds`** /
**`gotcha_uptime_watchdog_tick_duration_seconds`** — when the uptime watchdog
last completed a heartbeat/reminder pass, and how long it took. A stale
timestamp means missed heartbeat checks and incident reminders are not being
evaluated — a heartbeat monitor can go silent indefinitely without ever
raising an incident. Default interval is 1 minute; a duration approaching it
means the watchdog is falling behind.

**`gotcha_host_registration_failures_total`** — background writes to the host
registry that failed. While this grows, host `last_seen` is not refreshed, so
silence incidents may be raised for machines that are alive; the cause is almost
always an unavailable PostgreSQL.

**`gotcha_host_registrations_rejected_total`** — new host names dropped by the
ceiling of 1000 hosts per project. A non-zero value means new machines stop
appearing in the Hosts section: either the fleet really did reach the ceiling, or
an identifier leaked into the host name (pods, autoscaling) and every instance
registers as a separate machine.

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

**`gotcha_export_pending_jobs`** / **`gotcha_export_oldest_pending_age_seconds`**
— depth of the error/event export queue (requests in `queued` or `running`
status) and the age of the oldest one. The age matters more than the depth —
it is the only number that tells "the queue is empty because every request
was finished" from "the queue is stuck because the worker isn't running or is
blocked on disk". These metrics are only published where the queue is
actually served: `--mode=ingest` has no export worker (there's nobody to hand
the file to), so these metrics are absent there too — that absence is
expected, not a fault.

**`gotcha_export_failed_jobs`** — export requests that exhausted every retry
and were closed as `failed`. A non-zero, growing value is exactly the closed
P0 scenario (mass request failures were only visible as `slog.Warn` on the
worker's tick — an operator only learned about them by checking the log): every
request usually fails for the same reason, typically export directory
permissions or an exhausted `GOTCHA_EXPORT_DISK_BUDGET_BYTES` (see the last
attempt's error in the UI's Exports section).

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

**`gotcha_storage_used_bytes{store="exports"}`** — how many bytes the error/
event export directory (`GOTCHA_EXPORT_DIR`) currently occupies. Unlike
`store="postgres"`, this isn't an abstract database size — it's the same
directory whose budget the export worker checks before every request
(`GOTCHA_EXPORT_DISK_BUDGET_BYTES`); before this metric, that directory was
the one piece of disk the application manages entirely on its own and had no
external visibility at all. Polled every 5 minutes, same as the neighboring
`gotcha_storage_*` metrics; only registered where the export queue is
actually served (see `gotcha_export_pending_jobs` above).

**`gotcha_web_cross_origin_rejected_total`** — POST requests rejected because
their `Origin`/`Referer` did not match `GOTCHA_BASE_URL` (cross-origin
protection for the UI). Occasional ticks are scanner noise; steady growth from
real users usually means `GOTCHA_BASE_URL` differs from the address the UI is
actually served on — for example, behind a proxy that rewrites the scheme.

**`gotcha_build_info`** — always 1; the version and mode are in the labels. Use
it to confirm what is actually deployed. The `stamped` label says whether the
build carries git metadata: `stamped="false"` means the image was built outside
`make`, its version string is the source default, and "deployed exactly what
you think" cannot be verified from it.

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
