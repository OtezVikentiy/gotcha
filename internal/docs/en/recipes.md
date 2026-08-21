# Monitoring Recipes

The "Recipes" section is ready-made monitoring for common services: **PostgreSQL, MariaDB, nginx, Redis and Docker**. A recipe is a single page with everything needed to get a service under observation in a few minutes: a ready OpenTelemetry Collector config with your project key already filled in, a live "data arrives" indicator, preconfigured charts over the receiver's metrics, and recommended thresholds created with one click as ordinary [metric alert rules](/docs/metric-alerts).

Recipes introduce no new entities or ingest channels: service metrics travel over the same OTLP ingest as [Metrics](/docs/metrics), the thresholds are regular alert rules, and notifications go to the same project channels as [Alerts](/docs/alerts).

## Where to find it

The chart icon in the left icon rail → "Metrics" → the "Recipes" sub-item (or go directly to `/projects/{id}/recipes`). The list shows a card per recipe with a data-status badge and a created-thresholds counter; each card leads to the recipe page at `/projects/{id}/recipes/{slug}`.

Viewing is open to anyone with access to the project — it is telemetry reading and a connection guide, not configuration. The button that creates the recommended thresholds requires a project operator (a project team member, or the organization's owner/admin — see [Roles and access](/docs/teams)).

## Connecting a service

The recipe page walks you through the steps:

1. **Install `otelcol-contrib`** on the server next to the service. It is the same [OpenTelemetry Collector Contrib](https://github.com/open-telemetry/opentelemetry-collector-releases) as in the [hosts](/docs/hosts) connection guide — the official `.deb`/`.rpm` packages install the `otelcol-contrib` systemd unit and a config at `/etc/otelcol-contrib/config.yaml`.
2. **Copy the config from the recipe page** (the "Copy config" button) and replace the contents of `/etc/otelcol-contrib/config.yaml` with it. The instance address and an active public project key are already filled in; replace the `CHANGE_ME` values with your own (see the per-service requirements below). If the project has no active public key, the page shows a hint to issue one in the project settings instead of the config. The exporter `endpoint` is the instance's **base** URL, without `/v1/metrics`: the `otlphttp` exporter appends the path itself (the same rule as in [Hosts](/docs/hosts)).
3. **Restart the collector** (`sudo systemctl restart otelcol-contrib`). Within a minute or two the badge on the page flips from "Waiting for data" to "Receiving data".

**How detection works.** The "Receiving data" badge means the recipe's signature metric (e.g. `postgresql.backends` for PostgreSQL) has data points within the last **15 minutes**. Detection is live: if the service or the collector goes silent for longer than that window, the badge flips back to "Waiting for data" — and during first-time setup that same badge is an honest signal that the export has not landed yet.

One config — one receiver: if a server runs several services from the recipes (or the collector already ships host metrics), merge the `receivers`/`processors` sections and pipelines into a single `config.yaml` rather than running several collectors.

## Per-service requirements

### PostgreSQL

- The config's `username`/`password` is a PostgreSQL monitoring user. It needs no access to your data, only statistics reads: create a dedicated user and grant it the `pg_monitor` role.
- The receiver's `postgresql.deadlocks` metric is **disabled by default**, so the snippet enables it explicitly (`postgresql.deadlocks: {enabled: true}`). Do not remove that line: without it the recommended critical deadlock threshold would never fire — the metric simply would not arrive.
- The snippet's `tls: {insecure: true}` assumes a local connection on the same host. For a remote database, set up TLS on the receiver instead of keeping `insecure: true`.

### MariaDB

- The recipe uses the collector's `mysql` receiver — it officially supports both MySQL (5.7–9.x) and MariaDB (10.5.x–11.x, LTS 11.4 and 11.8), so the recipe works for either.
- The config's `username`/`password` is a MariaDB monitoring user. It needs no access to your data, only statistics reads: most metrics come from `SHOW GLOBAL STATUS`, and the performance-schema-based metrics need `GRANT SELECT ON performance_schema.*`.
- The receiver's `mysql.query.slow.count` metric is **disabled by default**, so the snippet enables it explicitly (`mysql.query.slow.count: {enabled: true}`). Do not remove that line: without it the recommended slow-queries threshold would never fire — the metric simply would not arrive.

### nginx

- The receiver reads the `stub_status` page — enable it in your nginx config:

```nginx
location /status {
    stub_status;
}
```

- The receiver `endpoint` in the snippet is `http://localhost:80/status`; adjust the host/port/path to match your server.

### Redis

- The config's `password` corresponds to your Redis `requirepass`. If Redis runs **without** `requirepass`, delete the `password` line from the config entirely (the receiver does not treat an empty value as "no password").

### Docker

- The `docker_stats` receiver reads the Docker socket `unix:///var/run/docker.sock` — the collector process needs access to it: run as root or as a member of the `docker` group.

## Why the config has a transform

The PostgreSQL and Docker snippets contain a `transform/recipe` processor — it is mandatory, do not remove it.

The reason: Gotcha's ingest keeps only `service.name`, `deployment.environment` and `host.name` out of the resource attributes and drops the rest (cardinality protection — see [Cardinality](/docs/cardinality)). But the receivers put the grouping keys exactly there, in resource attributes: the `postgresql` receiver puts the database name (`postgresql.database.name`), and `docker_stats` puts the container name (`container.name`; each container is a separate Resource for it). The `transform` processor promotes these attributes into datapoint attributes while still inside the collector, before export — and that is the only reason the "per database" and "per container" charts split into series. Remove the transform and those charts collapse into a single unnamed series.

MariaDB, nginx and Redis need no transform: everything their charts group by (e.g. `state` on nginx connections, or `kind`/`operation` on MariaDB metrics) are native datapoint attributes of the metrics themselves.

## Assumption: one service instance per project

A recipe assumes **one instance of the service per project**: metrics from two PostgreSQL servers (or two nginx, Redis…) arriving into one project without distinct `deployment.environment` values merge into a single series — charts and thresholds would compute over intermingled data. Spread multiple instances of the same service across environments (`resourcedetection`/`OTEL_RESOURCE_ATTRIBUTES` with distinct `deployment.environment`) or across separate projects.

The same applies to sub-resources within a single instance, not just to instances. One PostgreSQL server usually hosts **several databases**: the charts split them apart (per-database grouping), but a recommended threshold is a single rule over the metric — the deadlock threshold, for example, effectively tracks the database with the largest accumulated counter. For a precise per-database threshold, uncomment the `databases:` line in the receiver config and narrow it to a single database, or split the databases across projects. Docker is per-container in the same way: that is exactly why the recipe ships no default thresholds at all.

## Charts

As soon as data arrives, the recipe page shows preconfigured charts — each service has its own set: for PostgreSQL it is connections per database, commit/rollback transactions, database sizes, block reads, deadlocks and live/dead rows; for MariaDB — threads by kind, InnoDB file operations, buffer pool pages, row operations and table locks; for nginx — requests per second and connections; for Redis — memory, clients, cache hit rate, commands and fragmentation; for Docker — CPU, memory and network per container.

The chart window is fixed to the **last 24 hours**; the global time-range picker does not affect these charts. Grouped charts show the largest groups and hide the rest with a hint. The "Open in metrics" link under a chart leads to the same metric in the [Metrics](/docs/metrics) section — with arbitrary time ranges, aggregations and labels available there.

While no data arrives, the charts block is not shown at all — before the first export it would consist of nothing but empty cards.

## Recommended thresholds

At the bottom of the page is the recipe's recommended thresholds table: metric, condition, window, severity, an explanation and a status ("Created" / "Will be created"). The **"Create recommended thresholds"** button (for operators) creates the missing ones in one action — as ordinary metric alert rules, which then live on `/projects/{id}/metrics/alerts` where you can tune or delete them (see [Metric Alerts](/docs/metric-alerts)).

| Recipe | Threshold | Default condition | Window | Severity |
|---|---|---|---|---|
| PostgreSQL | Deadlocks | new deadlocks appeared (increase > 0) | 5 min | critical |
| PostgreSQL | Connections | more than 80 backends on average | 5 min | warning |
| MariaDB | Connected threads | more than 120 on average (`kind=connected`) | 5 min | warning |
| MariaDB | Slow queries | new slow queries appeared (increase > 0) | 5 min | warning |
| nginx | Active connections | more than 1000 on average (`state=active`) | 5 min | warning |
| Redis | Rejected connections | rejections appeared (increase > 0) | 5 min | critical |
| Redis | Fragmentation | ratio above 1.5 on average | 10 min | warning |
| Redis | Blocked clients | more than 5 on average | 5 min | warning |

What matters here:

- **Thresholds on counters mean "increase over the window"**, not a total count: "new deadlocks appeared within 5 minutes", not "total deadlocks above zero". The defaults with a 0 threshold catch every new occurrence.
- **The numeric defaults are a starting point**: tune the 80 PostgreSQL connections and the 120 MariaDB threads to your `max_connections`, and the 1000 nginx connections to your server's capacity. The explanation next to each threshold in the table says exactly what to adjust.
- **Creation is idempotent.** A rule with the same metric, aggregation, condition and label counts as "the same one" even if you have already tuned its threshold or window — pressing the button again neither overwrites nor duplicates it, it is simply skipped. Rules are created for all environments at once (the "Environment" field is empty); your own rule scoped to a specific environment does not block the default.
- **Docker has no recommended thresholds** — its metrics are per-container, and no sensible one-size-fits-all default over "all containers at once" exists. Configure rules for your own containers manually on the metric alerts page; the recipe page says so openly.

Incidents, notifications, the recovery hysteresis and the evaluator behave for the created rules exactly as for any metric alert rule: see [Metric Alerts](/docs/metric-alerts).

## What's next

- [Metrics](/docs/metrics) — the OTLP ingest recipes are built on; the metric detail page.
- [Metric Alerts](/docs/metric-alerts) — how the created thresholds are evaluated and notify.
- [Hosts](/docs/hosts) — system metrics of the servers themselves; installing the same collector.
- [Cardinality](/docs/cardinality) — why ingest keeps only a subset of resource attributes.
