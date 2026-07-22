[English](README.md) · [Русский](README.ru.md)

# Gotcha

Gotcha is a self-hosted observability platform: error tracking, performance
tracing, metrics, uptime monitoring, and profiling in one Go binary backed by
PostgreSQL and ClickHouse. It accepts data via the Sentry SDK ingestion
protocol and via OTLP (metrics), so it interoperates with the existing Sentry
SDK ecosystem — point an official Sentry SDK at a Gotcha project DSN and it
works.

## Features

- **Issues / error tracking** — event ingestion via the Sentry protocol, automatic grouping into issues, stack traces, breadcrumbs, tags/contexts.
- **Performance / tracing** — distributed traces and transactions, Web Vitals, performance-issue detection, regression detection.
- **Metrics** — ingestion via OTLP, metric queries, threshold-based alert rules and incidents.
- **Profiling** — CPU/flamegraph profiles from Sentry profiling payloads and pprof, with regression detection.
- **Uptime monitoring** — HTTP checks from a built-in local region or remote probes, incident detection, public status pages.
- **Alerting** — delivery via email, webhook, and Telegram; rules for new issues, spikes, metric thresholds, and performance/uptime regressions.
- **Organizations, teams and RBAC** — multi-tenant organizations, projects, membership roles.
- **SSO** — OIDC (generic), Yandex ID, and VK ID login, each independently configurable.
- **Privacy by default** — server-side PII scrubbing (IP/email zeroing, key-based redaction) and SSRF protection for outbound webhook/uptime requests, both on by default.

## Architecture

Gotcha ships as a single binary, `gotcha`, with a `--mode` flag that selects
which subsystems run in a given process:

- `--mode=ingest` — HTTP ingestion (Sentry envelope endpoints, OTLP metrics), event/span/metric/profile batching, alert evaluation on ingested data.
- `--mode=web` — the SSR web UI (templ + htmx), auth, org/project administration, dashboards, public status pages.
- `--mode=uptime` — the uptime check runner, incident watchdog, performance-regression and metric-threshold evaluators.
- `--mode=probe` — a remote uptime probe: talks only to a central Gotcha instance over HTTP, no direct database access.
- `--mode=all` (default) — everything above in a single process; the natural mode for a small/self-hosted install.

Storage: PostgreSQL holds relational state (organizations, projects, users,
issues, alert rules, incidents); ClickHouse holds high-volume event/span/
metric/profile/uptime-result data, with configurable per-signal retention.

## Quick start

Requires Docker and Docker Compose.

```bash
# gitflic (main, guaranteed anonymous HTTPS)
git clone https://gitflic.ru/project/otezvikentiy/gotcha.git
# GitHub (mirror, if published)
git clone https://github.com/OtezVikentiy/gotcha.git
# Contributors with SSH access can use:
# git clone git@gitflic.ru:otezvikentiy/gotcha.git
cd gotcha
docker compose up -d
```

This builds the `gotcha` image and starts three containers: `gotcha` (the app,
`--mode=all`), `postgres`, and `clickhouse`. The app listens on
`http://localhost:59080` by default (the compose file maps host port 59080 to
the container's 8080 to avoid clashing with other local stacks). Override the
host port with `GOTCHA_PORT`, e.g. `GOTCHA_PORT=8080 docker compose up -d`.
Equivalent `make` targets exist: `make up`, `make logs`, `make down`, `make
health` (see the Makefile for the full list).

Wait for the app to come up:

```bash
curl -sf http://localhost:59080/healthz
```

### First run

1. Open `http://localhost:59080/register` and create the first user. On a
   fresh instance, the first registered user always succeeds regardless of
   `GOTCHA_REGISTRATION` and is automatically granted instance-admin
   ("bootstrap" — see `GOTCHA_REGISTRATION` below for how later signups are
   gated).
2. Create an organization and a project from the UI.
3. Open the project's **Settings → Connect** page for its DSN and per-language
   snippets.
4. Point an official Sentry SDK at that DSN — Gotcha speaks the Sentry
   ingestion protocol, so any language's Sentry SDK works unmodified, e.g.:

   ```go
   sentry.Init(sentry.ClientOptions{Dsn: "<YOUR_PROJECT_DSN>"})
   ```

   Trigger an error in your app; it shows up under **Issues** within seconds.

More in-product documentation is available at `/docs` once the app is
running (also in source under `internal/docs/{en,ru}/`), including
[Getting Started](internal/docs/en/getting-started.md),
[SDK & Integrations](internal/docs/en/sdk.md), and per-section guides for
issues, performance, metrics, uptime, alerts, and profiling.

## Configuration

Gotcha is configured entirely through `GOTCHA_*` environment variables (see
`cmd/gotcha/config.go` for the authoritative source). Copy `.env.example` to
`.env` and adjust as needed; the most important variables:

| Variable | Default | Purpose |
|---|---|---|
| `GOTCHA_ADDR` | `:8080` | HTTP listen address. |
| `GOTCHA_BASE_URL` | `http://localhost:8080` | Public URL of this instance; used to build project DSNs, alert links, and invite links. Must match how users reach the instance. |
| `GOTCHA_PG_DSN` | `postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable` | PostgreSQL connection string. |
| `GOTCHA_CH_DSN` | `clickhouse://localhost:9000/gotcha` | ClickHouse connection string. |
| `GOTCHA_SECRET_KEY` | `insecure-dev-secret` | Signs OAuth state/session cookies. **The default is public** (it's in the source) and enables account takeover via OAuth on any non-localhost deployment — the process refuses to start in `web`/`all` mode on a non-local `GOTCHA_BASE_URL` unless this is overridden (escape hatch: `GOTCHA_ALLOW_INSECURE_SECRET=1`, dev only). Generate a strong random value for any real deployment. |
| `GOTCHA_SMTP_HOST` / `_PORT` / `_USER` / `_PASSWORD` / `_FROM` | unset / `587` / unset / unset / unset | Outbound email for invites and email alert channels; email delivery is disabled until `GOTCHA_SMTP_HOST` is set. |
| `GOTCHA_RETENTION_DAYS` | `90` | Retention for events/transactions/Web Vitals in ClickHouse. |
| `GOTCHA_SPAN_RETENTION_DAYS` | `30` | Retention for trace spans. |
| `GOTCHA_METRIC_RETENTION_DAYS` | `30` | Retention for metric points. |
| `GOTCHA_PROFILE_RETENTION_DAYS` | `7` | Retention for profile samples. |
| `GOTCHA_EDITION` | `oss` | `oss` or `saas`. Controls the default for the quota variables below (`oss` → 0/unlimited, `saas` → 1,000,000/month). |
| `GOTCHA_DEFAULT_EVENT_QUOTA` / `_TRANSACTION_QUOTA` / `_METRIC_QUOTA` / `_PROFILE_QUOTA` | `0` in `oss` (unlimited) | Default monthly ingest quota assigned to new organizations. **If you expose a project DSN publicly, set these to a real cap** — `oss` defaults to unlimited. |
| `GOTCHA_REGISTRATION` | `invite` | `open` (anyone can self-register), `invite` (self-registration closed except invite links), or `closed` (no self-registration at all). The very first user always succeeds regardless of this setting (instance-admin bootstrap). |
| `GOTCHA_SCRUB_IP` / `GOTCHA_SCRUB_EMAIL` | `true` / `true` | Zero out the reporting user's IP/email server-side before storage. On by default. |
| `GOTCHA_SCRUB_KEYS` | built-in denylist (`password`, `token`, `secret`, `authorization`, `cookie`, `api_key`, `access_token`, `refresh_token`, `session`, `credit_card`, `card_number`, `cvv`, …) | Comma-separated key names redacted from tags/contexts/stack traces/span data. Setting this overrides the built-in list entirely. |
| `GOTCHA_SSRF_ALLOW_PRIVATE` | `false` | Allow uptime checks and outbound webhooks to target private/loopback/link-local addresses. Keep `false` on any multi-tenant instance. |
| `GOTCHA_OIDC_ENABLED` / `GOTCHA_YANDEX_ENABLED` / `GOTCHA_VK_ENABLED` | `false` | Enable each SSO provider independently; each requires its own client ID/secret (and issuer, for OIDC) once enabled. |

Build-from-source variables (`GOTCHA_MAX_EVENT_BYTES`,
`GOTCHA_METRIC_EVAL_INTERVAL`, `GOTCHA_PROFILE_EVAL_INTERVAL`,
`GOTCHA_OUTBOX_RETENTION_DAYS`, `GOTCHA_AUTO_MIGRATE`,
`GOTCHA_EXTERNAL_CHANNEL_DETAILS`, `GOTCHA_UPTIME_CONCURRENCY`,
`GOTCHA_LOCAL_REGION`, `GOTCHA_PROBE_TOKEN`, `GOTCHA_SERVER_URL`,
`GOTCHA_SCRUB_FREETEXT`) and the full OIDC/Yandex/VK variable set are listed
with their defaults in [`.env.example`](.env.example).

## Build from source

Requires Go 1.26+.

```bash
go build -o gotcha ./cmd/gotcha
# or, via make:
make go-build
```

Running locally still needs PostgreSQL and ClickHouse. The bundled
`docker-compose.yml` only publishes the app's port to the host (Postgres and
ClickHouse are reachable from the `gotcha` container over the compose
network, not from your host by default). For source-level development,
either point `GOTCHA_PG_DSN`/`GOTCHA_CH_DSN` at your own local Postgres 17 /
ClickHouse 25.3 instances, or publish their ports yourself (e.g. a
git-ignored `docker-compose.override.yml` adding `ports: ["5432:5432"]` /
`ports: ["9000:9000"]` to those two services), then:

```bash
go run ./cmd/gotcha
```

To regenerate templ-generated Go files after editing a `.templ` file:

```bash
make templ
```

## Documentation, contributing, security

- In-product docs: `/docs` once running, or read them directly under [`internal/docs/en/`](internal/docs/en/) and [`internal/docs/ru/`](internal/docs/ru/).
- Contributing guide: [CONTRIBUTING.md](CONTRIBUTING.md).
- Reporting a vulnerability: [SECURITY.md](SECURITY.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE).
