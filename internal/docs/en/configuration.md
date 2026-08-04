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
| `GOTCHA_BASE_URL` | `http://localhost:8080` | The public address of your instance — how users and SDKs actually reach it. Used to build project DSNs, links in invite emails, and incident links in alerts (Telegram/webhook/email). Must **exactly match** the scheme+host+port the instance is really reachable at. If it's not `localhost`/`127.0.0.1`, the app requires a non-default `GOTCHA_SECRET_KEY` in the `web`, `all`, `ingest`, and `uptime` modes (everywhere except `probe`) — see the Security section below. If it doesn't start with `https://` and isn't local, a warning is logged (session cookies travel in plain text). |

## Database

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_PG_DSN` | `postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable` | PostgreSQL connection string — stores organizations, projects, users, alert rules, incidents. The stock `docker-compose.yml` already sets `postgres://gotcha:gotcha@postgres:5432/gotcha?sslmode=disable` (hostname `postgres` is the service name inside the Docker network). Only change this if you're using an external/your own database instead of the compose container. |
| `GOTCHA_CH_DSN` | `clickhouse://localhost:9000/gotcha` | ClickHouse connection string — stores events, trace spans, metrics, profiles, uptime check results. The stock compose file sets `clickhouse://gotcha:gotcha@clickhouse:9000/gotcha`. Change it for the same reasons as `GOTCHA_PG_DSN`. |

### Compose-only variables (database containers)

These four are **Docker Compose substitution variables**, not configuration of the gotcha process: the app never reads them. Compose substitutes them into the database containers' settings and into the DSNs above.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_PG_PASSWORD` | `gotcha` | Password of the `gotcha` PostgreSQL user. Substituted into both the `postgres` container (`POSTGRES_PASSWORD`) and the app's `GOTCHA_PG_DSN`. URL-unsafe characters (`@` `/` `:` `#` `%`) must not be used — the value goes into a DSN URL as-is. |
| `GOTCHA_CH_PASSWORD` | `gotcha` | Password of the `gotcha` ClickHouse user. Same mechanics and same character restriction as `GOTCHA_PG_PASSWORD`. |
| `GOTCHA_PG_MEM_LIMIT` | `512m` | Memory ceiling of the `postgres` container. Raise on a server with headroom. |
| `GOTCHA_CH_MEM_LIMIT` | `2g` | Memory ceiling of the `clickhouse` container. Without a cgroup limit ClickHouse assumes 90% of the **host's** memory is its own — the ceiling is what makes its memory budget real. |

**Changing a database password on an existing install.** `POSTGRES_PASSWORD`/`CLICKHOUSE_PASSWORD` only take effect when the data volume is first initialized — on a live install, changing the variable alone locks the app out of a database that still expects the old password. The order matters:

1. Change the password in the database itself:
   ```bash
   docker compose exec postgres psql -U gotcha -d gotcha -c "ALTER USER gotcha WITH PASSWORD 'new-password'"
   docker compose exec clickhouse clickhouse-client --user gotcha --password 'old-password' -q "ALTER USER gotcha IDENTIFIED BY 'new-password'"
   ```
2. Set `GOTCHA_PG_PASSWORD`/`GOTCHA_CH_PASSWORD` in `.env`.
3. `docker compose up -d` — the app container is recreated with the new DSN.

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
| `GOTCHA_RETENTION_DAYS` | `90` | Retention for events (errors), transactions, and Web Vitals, and for the summary records in PostgreSQL that describe them: error issues, performance issues, and performance regressions (see [Privacy](/docs/privacy)). Other summary records follow the retention of THEIR own telemetry: metric incidents `GOTCHA_METRIC_RETENTION_DAYS`, profile regressions `GOTCHA_PROFILE_RETENTION_DAYS`, resolved uptime incidents `GOTCHA_INCIDENT_RETENTION_DAYS`. Uptime check results (`check_results`) share this retention as well, so a value below 90 also shortens the public status page history: it shows at most this many days (up to its usual 90). `0` keeps data forever: the ClickHouse TTL is removed and nothing is deleted by age. |
| `GOTCHA_SPAN_RETENTION_DAYS` | `30` | Retention for trace spans (the detail inside transactions). `0` keeps data forever. |
| `GOTCHA_METRIC_RETENTION_DAYS` | `30` | Retention for metric points (ingested via OTLP). `0` keeps data forever. |
| `GOTCHA_PROFILE_RETENTION_DAYS` | `7` | Retention for profiling samples (the heaviest data by volume, hence the shorter default). `0` keeps data forever. |
| `GOTCHA_INCIDENT_RETENTION_DAYS` | `90` | Retention for RESOLVED uptime incidents in PostgreSQL. Its own setting rather than one shared with events: an uptime incident has no telemetry of its own in ClickHouse (check results have their own retention), while the public status page promises ninety days of history. Open incidents are never deleted. `0` keeps resolved incidents forever. |
| `GOTCHA_PURGE_RECONCILE_HOURS` | `24` | How often to look for ClickHouse telemetry of projects that no longer exist and queue it for deletion. Deleting a project queues that work itself, in the same transaction that removes the row; this check covers the case where no request was ever queued — a crash before commit, a manual row edit, data left over from earlier versions. `0` turns the check off, which an installation needs when something other than gotcha writes into the same ClickHouse. |
| `GOTCHA_OUTBOX_RETENTION_DAYS` | `7` | Retention for records of already-delivered/failed notifications (email/webhook/Telegram) in PostgreSQL. Deliberately short: this is a working queue rather than an archive: it lives in PostgreSQL and grows with notification volume. Must be at least 1 — `0` is rejected at startup. |

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
| `GOTCHA_INGEST_RATE_LIMIT` | `500` | Per-DSN ingest rate limit: requests per second per project (token bucket, burst = 2× the limit). Checked after DSN authentication and before quotas; over the limit the API replies `429` with a short `Retry-After`. `0` disables the limit. |
| `GOTCHA_MAX_BUFFER_BYTES` | `268435456` (256 MiB) | Byte ceiling for EACH ClickHouse writer buffer (events, spans, metrics, profiles, check results). Buffers grow while ClickHouse is unavailable so a short outage does not lose telemetry; this bounds the price of that. Five writers at the default add up to 1.25 GiB, so on a 2 GB server lower it — see `docker-compose.small.yml`. |
| `GOTCHA_MAX_QUEUE_BYTES` | `67108864` (64 MiB) | Byte ceiling for the ingest queue, on top of its capacity of 1000 tasks. A single event carries up to four raw JSON blocks of 256 KiB each — up to a megabyte — so without this the queue could hold roughly a gigabyte. On exhaustion the event is dropped and counted in `gotcha_pipeline_dropped_tasks_total`; the current size is `gotcha_pipeline_queued_bytes`. |

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
| `GOTCHA_SCRUB_KEYS` | built-in list (`password,passwd,token,secret,authorization,auth,cookie,api_key,apikey,access_token,refresh_token,session,credit_card,card_number,cvv`) | Comma-separated, case-insensitive key names whose values get redacted in tags/contexts/stack traces/span data. This variable **extends** the built-in list rather than replacing it, so adding your own key (e.g. `internal_user_id`) takes one value and does not require re-listing the standard ones. To drop a specific built-in key, name it exactly in `GOTCHA_SCRUB_ALLOW_KEYS`. |
| `GOTCHA_SCRUB_ALLOW_KEYS` | empty | Comma-separated exact names to exempt from the denylist. Matching is deliberately fail-closed — a name is redacted when it *contains* a denylist word, so `author` (contains `auth`) and `tokenizer` (contains `token`) are redacted by default. Under-redacting leaks personal data; over-redacting only costs a debugging field, and this setting brings it back: `GOTCHA_SCRUB_ALLOW_KEYS=author,tokenizer`. |
| `GOTCHA_SCRUB_FREETEXT` | `false` | Additionally masks email addresses found in free text (error message, exception value, span description). Off by default on purpose: naive masking can corrupt SQL or URLs embedded in error text. Only emails are masked, not phone numbers or other kinds of personal data. |


## Limits and evaluators

Ceilings that protect the instance from a flood, plus how often the background evaluators run. These bound WORK, not storage — which is why they no longer sit under Retention and Quotas.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_NOTIFY_CONCURRENCY` | `4` | How many notifications are delivered at once. One slow channel (a dead webhook waits up to 30 seconds) no longer holds up the rest. |
| `GOTCHA_ALERT_BUDGET_WINDOW_SECONDS` | `3600` | Window of the per-project notification ceiling. |
| `GOTCHA_ALERT_BUDGET_LIMIT` | `50` | How many notifications a project may send within that window. Rule throttling keys on (issue, rule), and a brand-new issue has no throttle row — so a sender using a unique `fingerprint` per event got one notification per event. Suppressed alerts are not lost: once the window closes a digest reports "N alerts suppressed". `0` disables the ceiling entirely. |
| `GOTCHA_CARDINALITY_LIMIT` | `10000` | Cap on DISTINCT values of open-ended fields (transaction name, environment, metric name, service, span operation) per project per window. Values beyond the cap are grouped under `<cardinality-limit>` rather than dropped; the affected page shows which field hit the cap and examples of the grouped values. The cause is almost always an identifier inside a name — see [Cardinality](/docs/cardinality). `0` removes the limit. |
| `GOTCHA_CARDINALITY_WINDOW_SECONDS` | `3600` | Window after which the set of distinct values starts fresh, so a project that fixed its names recovers on its own. |
| `GOTCHA_RUN_EVALUATORS` | by mode | Whether to run the periodic evaluators: performance regressions, metric alert rules, profile regressions. They ship with `uptime` and `all` by default, although they have nothing to do with uptime. In a `web`+`ingest` split (no uptime in use) a metric alert rule looks enabled and **never fires** — set this on exactly one replica. A warning is logged at startup in modes without evaluators. |
| `GOTCHA_METRIC_EVAL_INTERVAL` | `60` | How often (seconds) metric threshold alert rules are evaluated. |
| `GOTCHA_PROFILE_EVAL_INTERVAL` | `300` | How often (seconds) the profiling regression detector runs. |

## Observability and logs

Detail level and format of the instance's own logs.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn` or `error`. Raise it to `debug` to get more detail during an incident without rebuilding. |
| `GOTCHA_LOG_FORMAT` | `text` | Log format: `text` or `json`. Use `json` when shipping logs to Loki/ELK. |

## Security

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_SECRET_KEY` | `insecure-dev-secret` | Signs session and OAuth state cookies. **The default is public** (it's in the source code) — leaving it on a real server allows account takeover via OAuth login. On a non-localhost `GOTCHA_BASE_URL`, the app refuses to start in the `web`, `all`, `ingest`, and `uptime` modes (everywhere except `probe`) until a real key is set, and requires it to be **at least 32 bytes** (a shorter key is a weak signature). Generate one with `openssl rand -hex 32`. Both checks can be waived for an unusual dev setup with `GOTCHA_ALLOW_INSECURE_SECRET=1`. See [Installation](/docs/installation), step 5, for the full walkthrough. |
| `GOTCHA_TRUSTED_PROXIES` | *(empty)* | Comma-separated CIDRs (`10.0.0.0/8`) and/or bare IPs (`192.168.1.5`, treated as `/32` or `/128`) of reverse proxies you trust. Behind a proxy (the recommended [install](/docs/installation) topology), set this to the proxy's address so the per-IP login rate limiter keys on the real client IP from `X-Forwarded-For` instead of the proxy's — otherwise every login attempt looks like it comes from the proxy and the limiter can't tell clients apart. Invalid entries are a startup error, not a silent skip. |
| `GOTCHA_ALLOW_INSECURE_SECRET` | `false` | Escape hatch that bypasses the check above — lets the app start with the default key even on a non-localhost address. **Development-only**; never set this on a real deployment. |
| `GOTCHA_REGISTRATION` | `invite` | Self-registration mode: `open` — anyone can register; `invite` — an account is created only against a valid invitation (by password or through a provider); `closed` — **no new accounts appear at all, not even with a valid invitation**. That is the difference: under `closed` an invited person hits "registration is closed", and inviting anyone from outside becomes impossible — invitations only work for people who already have an account. The very first user on a fresh instance can always register regardless of this setting (instance-admin bootstrap). |
| `GOTCHA_LOCALE` | `ru` | Language of outgoing notifications (uptime, regressions, performance) delivered by email/Telegram/webhook: `ru` or `en`. Recipients outside the UI have no per-user locale, so the instance operator picks one for the whole instance. Does not affect the web UI — there every user chooses their own language. |
| `GOTCHA_SSRF_ALLOW_PRIVATE` | `false` | Allow uptime checks and outbound webhook alert deliveries to target private/loopback/link-local addresses (e.g. `192.168.x.x`, `127.0.0.1`, `169.254.x.x`). Keep this `false` on any instance shared across multiple users/organizations — otherwise one user could set up an "uptime check" or webhook that actually probes your internal network (SSRF). |
| `GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME` | inherits `GOTCHA_SSRF_ALLOW_PRIVATE` | Allow uptime checks to reach private/loopback addresses. Usually the only one you need: monitoring an internal service is routine, and the target is set by an organization admin. |
| `GOTCHA_SSRF_ALLOW_PRIVATE_WEBHOOK` | inherits `GOTCHA_SSRF_ALLOW_PRIVATE` | Allow alert webhooks to reach private addresses. Riskier: up to 1 KB of the target's response is shown on the deliveries page, which turns a webhook into a reader of internal services. |
| `GOTCHA_SSRF_ALLOW_PRIVATE_OIDC` | inherits `GOTCHA_SSRF_ALLOW_PRIVATE` | Allow OIDC discovery/token calls to reach private addresses — needed for an internal IdP. Riskiest: the client secret is sent to the token endpoint taken from the discovery document. |
| `GOTCHA_AUTO_MIGRATE` | `true` | Apply database schema migrations automatically on startup. `false` means migrations must be applied as a separate step beforehand — otherwise the app refuses to start against a schema that's out of date. See [Upgrade](/docs/upgrade) for details and when this is needed. |
| `GOTCHA_EXTERNAL_CHANNEL_DETAILS` | `false` | Whether to send error text (title/culprit/body) to external alert channels (Telegram/webhook). `false` sends only an anonymized link back to the instance, without the error text (which may contain personal data you don't want leaving the instance). |
| `GOTCHA_TRUSTED_RECIPIENTS` | empty | Comma-separated domains and hosts of your own perimeter: mail on these domains and webhooks on these hosts receive event details even with `GOTCHA_EXTERNAL_CHANNEL_DETAILS` off. Matching is on label boundaries (`corp.example` covers `mail.corp.example`, not `evilcorp.example`). The instance host from `GOTCHA_BASE_URL` and internal-network addresses are always trusted, with no configuration. See [Privacy and 152-FZ](/docs/privacy). |

> The `--migrate-only` command-line flag applies the schema and exits without starting any component: an init job for deployments with `GOTCHA_AUTO_MIGRATE=false`. See [Upgrade](/docs/upgrade).

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
| `GOTCHA_OIDC_SCOPES` | `openid email profile` | The **complete** space-separated scope list sent to the provider. Setting it **replaces** the default rather than adding to it, so always include `openid` and `email` — without them the ID token carries no subject or address and every login fails. To request an extra scope, list it alongside the defaults: `openid email profile groups`. |
| `GOTCHA_OIDC_NAME` | *(empty)* | Display name for the login button ("Sign in with …") in the UI. |
| `GOTCHA_YANDEX_ENABLED` | `false` | Enables login via Yandex ID. Requires `GOTCHA_YANDEX_CLIENT_ID`/`_CLIENT_SECRET`. |
| `GOTCHA_VK_ENABLED` | `false` | Enables login via VK ID. Requires `GOTCHA_VK_CLIENT_ID`/`_CLIENT_SECRET`. |

Step-by-step setup for each provider is in [SSO](/docs/sso).

## What's next

- [Installation](/docs/installation) — getting started on a fresh server.
- [Backup & Restore](/docs/backup-restore).
- [Upgrade](/docs/upgrade).
- [SSO](/docs/sso).
