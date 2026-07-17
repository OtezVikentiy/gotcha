# Configuration

Gotcha is configured entirely through environment variables prefixed `GOTCHA_`. There's no config file and no web UI screen for system-level settings — only environment variables. Their authoritative source is `cmd/gotcha/config.go` in the source tree; this document is a readable description of the same thing, grouped by purpose. A template with every variable and comments lives in `.env.example` at the repository root.

## How to set environment variables with Docker Compose

Two equivalent ways:

**Option 1 — an `.env` file next to `docker-compose.yml`** (recommended, the simplest). Docker Compose reads it automatically:

```bash
# inside the gotcha/ directory
nano .env
```

```env
GOTCHA_SECRET_KEY=random-string-from-openssl-rand
GOTCHA_BASE_URL=https://gotcha.example.com
GOTCHA_SMTP_HOST=smtp.example.com
GOTCHA_SMTP_PORT=465
GOTCHA_SMTP_USER=noreply@example.com
GOTCHA_SMTP_PASSWORD=an-app-password
GOTCHA_SMTP_FROM=noreply@example.com
```

Apply the changes:

```bash
docker compose up -d
```

(this recreates the `gotcha` container with the new variables; it leaves `postgres`/`clickhouse` alone unless their own variables changed).

**Option 2 — an `environment:` block right in `docker-compose.yml`.** If you'd rather not use an `.env` file, you can set variables directly in the compose file, under the `gotcha` service:

```yaml
services:
  gotcha:
    # ...
    environment:
      GOTCHA_PG_DSN: postgres://gotcha:gotcha@postgres:5432/gotcha?sslmode=disable
      GOTCHA_CH_DSN: clickhouse://gotcha:gotcha@clickhouse:9000/gotcha
      GOTCHA_BASE_URL: ${GOTCHA_BASE_URL:-http://localhost:59080}
      GOTCHA_SECRET_KEY: ${GOTCHA_SECRET_KEY:-insecure-dev-secret}
      GOTCHA_SMTP_HOST: smtp.example.com
```

`${VAR:-default}` is Docker Compose interpolation: "take `VAR` from the environment/`.env`, and if it isn't set, use the value after `:-`." The repository's stock `docker-compose.yml` already uses this pattern for `GOTCHA_BASE_URL` and `GOTCHA_SECRET_KEY`, so for those two, option 1 (just creating an `.env` file) is usually enough — no need to edit the compose file itself.

After changing any variable, run `docker compose up -d` to apply it — Docker Compose detects that the container's configuration changed and recreates it.

---

## Core

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_ADDR` | `:8080` | The address and port the HTTP server listens on **inside the container**. You normally don't need to change this — the port is published to the host via `docker-compose.yml`/`GOTCHA_PORT` instead (see [Installation](/docs/installation)), not via this variable. |
| `GOTCHA_BASE_URL` | `http://localhost:8080` | The public address of your instance — how users and SDKs actually reach it. Used to build project DSNs, links in invite emails, and incident links in alerts (Telegram/webhook/email). Must **exactly match** the scheme+host+port the instance is really reachable at. If it's not `localhost`/`127.0.0.1`, the app requires a non-default [`GOTCHA_SECRET_KEY`](#security) in `web`/`all` mode — see below. If it doesn't start with `https://` and isn't local, a warning is logged (session cookies travel in plain text). |

## Database

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_PG_DSN` | `postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable` | PostgreSQL connection string — stores organizations, projects, users, alert rules, incidents. The stock `docker-compose.yml` already sets `postgres://gotcha:gotcha@postgres:5432/gotcha?sslmode=disable` (hostname `postgres` is the service name inside the Docker network). Only change this if you're using an external/your own database instead of the compose container. |
| `GOTCHA_CH_DSN` | `clickhouse://localhost:9000/gotcha` | ClickHouse connection string — stores events, trace spans, metrics, profiles, uptime check results. The stock compose file sets `clickhouse://gotcha:gotcha@clickhouse:9000/gotcha`. Change it for the same reasons as `GOTCHA_PG_DSN`. |

## Email / SMTP

Used for invite emails and the email alert channel. As long as `GOTCHA_SMTP_HOST` is empty, email sending is disabled entirely (the log shows `GOTCHA_SMTP_HOST is not set, email alert channels are disabled`), while everything else keeps working normally.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_SMTP_HOST` | *(empty)* | The SMTP server address, e.g. `smtp.example.com`. Email is disabled while this is empty. |
| `GOTCHA_SMTP_PORT` | `587` | SMTP port. `587` (STARTTLS) is the common choice; some providers use `465` (SMTPS). |
| `GOTCHA_SMTP_USER` | *(empty)* | Login used to authenticate to the SMTP server. |
| `GOTCHA_SMTP_PASSWORD` | *(empty)* | Password. Providers like Gmail/Yandex typically require a separate "app password" rather than your account password. |
| `GOTCHA_SMTP_FROM` | *(empty)* | Sender address used in the `From:` header of emails. |

## Retention (ClickHouse data)

How many days ClickHouse keeps each kind of data before deleting old rows. Lower means less disk usage; higher means a deeper history for investigations.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_RETENTION_DAYS` | `90` | Retention for events (errors), transactions, and Web Vitals. |
| `GOTCHA_SPAN_RETENTION_DAYS` | `30` | Retention for trace spans (the detail inside transactions). |
| `GOTCHA_METRIC_RETENTION_DAYS` | `30` | Retention for metric points (ingested via OTLP). |
| `GOTCHA_PROFILE_RETENTION_DAYS` | `7` | Retention for profiling samples (the heaviest data by volume, hence the shorter default). |
| `GOTCHA_OUTBOX_RETENTION_DAYS` | `7` | Retention for records of already-delivered/failed notifications (email/webhook/Telegram) in PostgreSQL. Deliberately short: these records carry delivery-channel secrets (e.g. a webhook token). |

Retention changes apply on the next application start (the value is used to set a TTL on the ClickHouse tables) — data already deleted doesn't come back retroactively.

## Quotas & edition

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_EDITION` | `oss` | `oss` or `saas`. Controls the **default** for the quota variables below: in `oss` all defaults are `0` (unlimited); in `saas`, `1,000,000`/month. An explicit `GOTCHA_DEFAULT_*_QUOTA` always overrides the edition default. |
| `GOTCHA_DEFAULT_EVENT_QUOTA` | `0` in oss | Default monthly ingest quota for events (errors) assigned to new organizations. `0` = unlimited. |
| `GOTCHA_DEFAULT_TRANSACTION_QUOTA` | `0` in oss | Same, for transactions (performance traces). |
| `GOTCHA_DEFAULT_METRIC_QUOTA` | `0` in oss | Same, for metric points. |
| `GOTCHA_DEFAULT_PROFILE_QUOTA` | `0` in oss | Same, for profiles. |
| `GOTCHA_MAX_EVENT_BYTES` | `1048576` (1 MiB) | Maximum size, in bytes, of a single ingested event. Larger events are rejected. |
| `GOTCHA_METRIC_EVAL_INTERVAL` | `60` | How often (seconds) metric threshold alert rules are evaluated. |
| `GOTCHA_PROFILE_EVAL_INTERVAL` | `300` | How often (seconds) the profiling regression detector runs. |

**When you must change the quotas:** `0` (unlimited) in the oss edition is a **deliberate choice for a private self-hosted instance** where the DSN never leaks. If a project's DSN ends up in publicly reachable code (e.g. your website's frontend JS), anyone can send it an unbounded volume of events — both an abuse vector and a risk of filling up ClickHouse's disk. In that case, set real numbers, e.g.:

```env
GOTCHA_DEFAULT_EVENT_QUOTA=100000
GOTCHA_DEFAULT_TRANSACTION_QUOTA=50000
```

(This is the default applied to *new* organizations; an existing organization's quota can be changed in its settings in the web UI.)

## Privacy / scrubbing

Server-side removal of personal data before storage — on by default.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_SCRUB_IP` | `true` | Zeroes the reporting user's IP address before storage. |
| `GOTCHA_SCRUB_EMAIL` | `true` | Zeroes the reporting user's email before storage. |
| `GOTCHA_SCRUB_KEYS` | built-in list (`password,passwd,token,secret,authorization,auth,cookie,api_key,apikey,access_token,refresh_token,session,credit_card,card_number,cvv`) | Comma-separated, case-insensitive key names whose values get redacted in tags/contexts/stack traces/span data. Setting this explicitly **completely replaces** the built-in list rather than extending it — if you want to add a key (e.g. `internal_user_id`), include the standard ones you still want alongside it. |
| `GOTCHA_SCRUB_FREETEXT` | `false` | Additionally masks email addresses found in free text (error message, exception value, span description). Off by default on purpose: naive masking can corrupt SQL or URLs embedded in error text. Only emails are masked, not phone numbers or other kinds of personal data. |

## Security

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_SECRET_KEY` | `insecure-dev-secret` | Signs session and OAuth state cookies. **The default is public** (it's in the source code) — leaving it on a real server allows account takeover via OAuth login. On a non-localhost `GOTCHA_BASE_URL`, the app refuses to start in `web`/`all` mode until a real key is set. Generate one with `openssl rand -base64 32`. See [Installation](/docs/installation), step 6, for the full walkthrough. |
| `GOTCHA_ALLOW_INSECURE_SECRET` | `false` | Escape hatch that bypasses the check above — lets the app start with the default key even on a non-localhost address. **Development-only**; never set this on a real deployment. |
| `GOTCHA_REGISTRATION` | `invite` | Self-registration mode: `open` — anyone can register; `invite` — self-registration is closed, new users can only join via an invite link; `closed` — self-registration is disabled entirely. The very first user on a fresh instance can always register regardless of this setting (instance-admin bootstrap). |
| `GOTCHA_SSRF_ALLOW_PRIVATE` | `false` | Allow uptime checks and outbound webhook alert deliveries to target private/loopback/link-local addresses (e.g. `192.168.x.x`, `127.0.0.1`, `169.254.x.x`). Keep this `false` on any instance shared across multiple users/organizations — otherwise one user could set up an "uptime check" or webhook that actually probes your internal network (SSRF). |
| `GOTCHA_AUTO_MIGRATE` | `true` | Apply database schema migrations automatically on startup. `false` means migrations must be applied as a separate step beforehand — otherwise the app refuses to start against a schema that's out of date. See [Upgrade](/docs/upgrade) for details and when this is needed. |
| `GOTCHA_EXTERNAL_CHANNEL_DETAILS` | `true` | Whether to send error text (title/culprit/body) to external alert channels (Telegram/webhook). `false` sends only an anonymized link back to the instance, without the error text (which may contain personal data you don't want leaving the instance). |

## Uptime & probe

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_UPTIME_CONCURRENCY` | `50` | How many uptime checks run concurrently (in `uptime`/`all` mode, and by a remote probe in `probe` mode). |
| `GOTCHA_LOCAL_REGION` | `local` | The name of the built-in local uptime-check region — what's shown in the UI when picking a monitor's region. |
| `GOTCHA_PROBE_TOKEN` | *(empty)* | `--mode=probe` only: the bearer token this probe authenticates to the central instance with. Required in this mode. |
| `GOTCHA_SERVER_URL` | *(empty)* | `--mode=probe` only: the base URL of the central Gotcha instance the probe reports to. Required in this mode, must be an absolute `http(s)` URL. |

`--mode=probe` is a separate process deployed in another region/data center: it doesn't open PostgreSQL or ClickHouse at all — it only makes outbound HTTP requests to the central instance.

## OAuth / SSO

Each login provider is enabled independently. Enabling a provider without setting its required secrets makes the app refuse to start.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_OIDC_ENABLED` | `false` | Enables login via a generic OIDC provider (Keycloak, Authentik, Google Workspace, etc). Requires `GOTCHA_OIDC_ISSUER`, `_CLIENT_ID`, `_CLIENT_SECRET`. |
| `GOTCHA_OIDC_ISSUER` | *(empty)* | The OIDC provider's issuer URL. |
| `GOTCHA_OIDC_CLIENT_ID` / `_CLIENT_SECRET` | *(empty)* | Credentials for the application registered with the provider. |
| `GOTCHA_OIDC_SCOPES` | *(empty)* | Extra OAuth scopes beyond the defaults, space/comma-separated per your provider's convention. |
| `GOTCHA_OIDC_NAME` | *(empty)* | Display name for the login button ("Sign in with …") in the UI. |
| `GOTCHA_YANDEX_ENABLED` | `false` | Enables login via Yandex ID. Requires `GOTCHA_YANDEX_CLIENT_ID`/`_CLIENT_SECRET`. |
| `GOTCHA_VK_ENABLED` | `false` | Enables login via VK ID. Requires `GOTCHA_VK_CLIENT_ID`/`_CLIENT_SECRET`. |

Step-by-step setup for each provider is in [SSO](/docs/sso).

## What's next

- [Installation](/docs/installation) — getting started on a fresh server.
- [Backup & Restore](/docs/backup-restore).
- [Upgrade](/docs/upgrade).
- [SSO](/docs/sso).
