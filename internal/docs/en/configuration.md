# Configuration

Gotcha is configured entirely through environment variables prefixed `GOTCHA_`. There's no config file and no web UI screen for system-level settings — only environment variables. Their authoritative source is `cmd/gotcha/config.go` in the source tree; this document is a readable description of the same thing, grouped by purpose. A template with every variable and comments lives in `.env.example` at the repository root.

## Naming convention

A numeric variable carries its unit right in the name: `_SECONDS`, `_DAYS`, `_HOURS`, `_BYTES`, `_PER_SEC`, `_PER_MIN` (for example `GOTCHA_ESCALATION_INTERVAL_SECONDS`, `GOTCHA_EXPORT_TTL_HOURS`, `GOTCHA_DIST_RATE_PER_MIN`). A bare number without a unit in the name is ambiguous — sixty of what, seconds or minutes? For retention settings the name also names the kind of data being cut off (`GOTCHA_EVENT_RETENTION_DAYS`, `GOTCHA_LOG_RETENTION_DAYS`, `GOTCHA_DEPLOY_RETENTION_DAYS`), rather than one shared `RETENTION_DAYS` for everything. The variable's prefix names the subsystem, not the product as a whole: `GOTCHA_AGENT_*` is the host agent process (`gotcha-agent`, a separate binary) only; `GOTCHA_DIST_*` is the instance serving agent binaries (not the agent itself); `GOTCHA_PROBE_*` is a remote probe (`--mode=probe`). An invalid value for any variable refuses to start the process rather than silently falling back to a default — a typo in the configuration must be caught by the operator immediately, not weeks later as silently wrong behavior.

The unit-naming convention is enforced by `internal/guards/env_example_test.go`: ANY variable read in `cmd/gotcha/config.go` or `internal/agent/config.go` as a bare number (`intNum`/`num`) is suspect by default — the gate fails it unless its name carries the matching suffix or it is listed explicitly in that test's closed `unitlessCounters` map (bare counters — limits, concurrency, a port — that have no unit to carry). Adding a new line to that list is a review-visible change, not a quiet way around the convention.

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
| `GOTCHA_PG_DSN` | `postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable` | PostgreSQL connection string — stores organizations, projects, users, alert rules, incidents. The stock `docker-compose.yml` already sets `postgres://gotcha:gotcha@postgres:5432/gotcha?sslmode=disable` (hostname `postgres` is the service name inside the Docker network). Only change this if you're using an external/your own database instead of the compose container. The value is trimmed; a whitespace-only value refuses to start instead of silently falling back to the default localhost DSN. |
| `GOTCHA_CH_DSN` | `clickhouse://localhost:9000/gotcha` | ClickHouse connection string — stores events, trace spans, metrics, profiles, uptime check results. The stock compose file sets `clickhouse://gotcha:gotcha@clickhouse:9000/gotcha`. Change it for the same reasons as `GOTCHA_PG_DSN`. Same trimming, same refusal to start on a whitespace-only value. |

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

### Compose-only variables (the app container)

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_MEM_LIMIT` | `1g` | Memory ceiling of the `gotcha` container. The app reads this limit from its cgroup and sets its own heap ceiling to 80% of it, so raising the ceiling is enough — `GOMEMLIMIT` doesn't need setting by hand. |
| `GOTCHA_NET_MTU` | `1500` | MTU of the container network. A last resort for one specific failure — see below; a mismatch on its own is not a reason to touch it. |

**Start with the symptom, not the numbers.** Docker gives container networks an MTU of 1500 without looking at the host's uplink, and a VPS behind a tunnel (GRE, VXLAN, OpenVZ) commonly has 1450. The mismatch itself is everywhere, and most installations live with it and never notice.

It is only harmless in one direction, though. Outbound packets are fixed by your own kernel: the narrow link is the host's own interface, the kernel says so, and the container lowers the route's MTU. Inbound packets depend on the **remote end**: it sends segments of the size the container advertised when the connection was set up (MSS = MTU − 40, so 1460 instead of 1410), and it only shrinks them if it receives an ICMP "fragmentation needed" from a router on the path. Whether that ICMP arrives is neither your side nor your control. Where it is filtered, the sender keeps pushing 1460-byte segments and they vanish.

Every sign of this failure follows from that:

- small exchanges work, large ones disappear: TCP connects, the SMTP `220` greeting arrives, and the TLS handshake — the first large exchange — hangs until it times out;
- **different destinations behave differently**: TLS completes to one host and hangs on another, because the ICMP is not filtered everywhere;
- **it can work and then stop**: the kernel keeps a learned path MTU in the route cache for about ten minutes (`net.ipv4.route.mtu_expires`), and while that entry lives everything is fine — once it expires the symptom returns on its own;
- from the host the very same thing always works: that interface is 1450, so the MSS is 1410 from the start and no ICMP is needed.

That last point is the trap when diagnosing this. Test from **inside the container**, not from the host:

```bash
docker compose exec gotcha wget -q -O /dev/null https://api.github.com/ && echo ok || echo fail
```

If TLS hangs there while `openssl s_client -starttls smtp -connect <your-smtp>:587` completes on the host, the MTU is your answer.

Everything the instance sends outward shares the path — email, webhook channels, OAuth, and uptime checks — so when this does happen, a monitored site can be reported unreachable because of the MTU of the container watching it.

Check the host:

```bash
ip -o link show
```

If the external interface (`ens3`, `eth0`) is below 1500 while `docker0` shows 1500 **and** you have the timeout above, set `GOTCHA_NET_MTU` to the interface's value and bring the stack back up. The container then advertises an MSS the path can certainly carry, and delivery stops depending on somebody else's ICMP:

```bash
docker compose down && docker compose up -d
```

`down` matters here — `up -d` alone is not enough. Changing the value requires recreating the network, and doing that under running containers leaves Docker's embedded DNS holding the old records: services stop finding each other by name (`lookup postgres on 127.0.0.11:53: server misbehaving`) until the whole stack is brought up again.

If delivery doesn't recover, the MTU wasn't the cause — put the value back and look at reachability instead ([Alerts](/docs/alerts) walks through it).

## Email / SMTP

Used for invite emails and the email alert channel. As long as `GOTCHA_SMTP_HOST` is empty, email sending is disabled entirely (the log shows `GOTCHA_SMTP_HOST is not set, email alert channels are disabled`), while everything else keeps working normally.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_SMTP_HOST` | *(empty)* | The SMTP server address, e.g. `smtp.example.com`. Email is disabled while this is empty. |
| `GOTCHA_SMTP_PORT` | `587` | SMTP port. `587` (STARTTLS) is the common choice; some providers use `465` (SMTPS). |
| `GOTCHA_SMTP_USER` | *(empty)* | Login used to authenticate to the SMTP server. |
| `GOTCHA_SMTP_PASSWORD` | *(empty)* | Password. Providers like Gmail/Yandex typically require a separate "app password" rather than your account password. |
| `GOTCHA_SMTP_FROM` | *(empty)* | Sender address used in the `From:` header of emails. |

## Telegram

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_TELEGRAM_API_BASE` | *(empty — `https://api.telegram.org`)* | Base address of the Bot API. Set it when the instance can't reach `api.telegram.org`: traffic filtering on the way out, a closed egress, or your own [`telegram-bot-api`](https://github.com/tdlib/telegram-bot-api) server. Must be an absolute `http(s)` URL with no query or fragment — the sender appends `/bot{token}/sendMessage`; an invalid value is a startup error, not a per-delivery timeout. |

Outbound HTTP proxies are honoured for Telegram through the standard `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` environment variables. They do **not** apply to webhook channels, OAuth or uptime checks: those dial their targets directly on purpose, because the SSRF filter decides on the address actually connected to, and a proxy would move that decision off the instance. See [Alerts](/docs/alerts) for the Telegram troubleshooting path.

## Retention (ClickHouse data)

How many days ClickHouse keeps each kind of data before deleting old rows. Lower means less disk usage; higher means a deeper history for investigations.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_EVENT_RETENTION_DAYS` | `90` | Retention for events (errors), transactions, and Web Vitals, and for the summary records in PostgreSQL that describe them: error issues, performance issues, and performance regressions (see [Privacy](/docs/privacy)). Other summary records follow the retention of THEIR own telemetry: metric incidents `GOTCHA_METRIC_RETENTION_DAYS`, profile regressions `GOTCHA_PROFILE_RETENTION_DAYS`, resolved uptime incidents `GOTCHA_INCIDENT_RETENTION_DAYS`. Uptime check results (`check_results`) share this retention as well, so a value below 90 also shortens the public status page history: it shows at most this many days (up to its usual 90). `0` keeps data forever: the ClickHouse TTL is removed and nothing is deleted by age. |
| `GOTCHA_SPAN_RETENTION_DAYS` | `30` | Retention for trace spans (the detail inside transactions). `0` keeps data forever. |
| `GOTCHA_METRIC_RETENTION_DAYS` | `30` | Retention for metric points (ingested via OTLP). `0` keeps data forever. |
| `GOTCHA_PROFILE_RETENTION_DAYS` | `7` | Retention for profiling samples (the heaviest data by volume, hence the shorter default). `0` keeps data forever. |
| `GOTCHA_LOG_RETENTION_DAYS` | `14` | Retention for structured logs (ingested via OTLP or NDJSON — see [Logs](/docs/logs)). Logs are more voluminous than events, hence the shorter default. `0` keeps data forever. |
| `GOTCHA_INCIDENT_RETENTION_DAYS` | `90` | Retention for RESOLVED uptime incidents in PostgreSQL. Its own setting rather than one shared with events: an uptime incident has no telemetry of its own in ClickHouse (check results have their own retention), while the public status page promises ninety days of history. Open incidents are never deleted. `0` keeps resolved incidents forever. |
| `GOTCHA_DEPLOY_RETENTION_DAYS` | `90` | Retention for deployment markers in PostgreSQL. Its own setting rather than one shared with events: deployment history is a separate axis with no telemetry of its own in ClickHouse, and the table is written by the public ingest key (CI posts deployments with the same DSN) outside any quota, so a bound is mandatory. `0` keeps deployment markers forever. |
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
| `GOTCHA_DEFAULT_LOG_QUOTA` | `0` in oss | Same, for logs. |
| `GOTCHA_MAX_EVENT_BYTES` | `1048576` (1 MiB) | Maximum size, in bytes, of a single ingested event. Larger events are rejected. |
| `GOTCHA_INGEST_RATE_PER_SEC` | `500` | Per-DSN ingest rate limit: requests per second per project (token bucket, burst = 2× the limit). Checked after DSN authentication and before quotas; over the limit the API replies `429` with a short `Retry-After`. `0` disables the limit. |
| `GOTCHA_MAX_BUFFER_BYTES` | auto (see below) | Byte ceiling for EACH ClickHouse writer buffer (events, spans, metrics, profiles, logs). Buffers grow while ClickHouse is unavailable so a short outage does not lose telemetry; this bounds the price of that. A value set here always wins over the auto-derived default. Uptime check results are buffered separately and capped by row count (10000), not by this variable. Leaving it unset enables the auto-derived behavior; an explicit `0` or negative number is a configuration error and refuses to start. |
| `GOTCHA_MAX_QUEUE_BYTES` | `67108864` (64 MiB) | Byte ceiling for the ingest queue, on top of its capacity of 1000 tasks. A single event carries up to four raw JSON blocks of 256 KiB each — up to a megabyte — so without this the queue could hold roughly a gigabyte. On exhaustion the event is dropped and counted in `gotcha_pipeline_dropped_tasks_total`; the current size is `gotcha_pipeline_queue_bytes`. Leaving it unset uses the default above; an explicit `0` or negative number is a configuration error and refuses to start. |

**`GOTCHA_MAX_BUFFER_BYTES` auto-default:** when the variable is unset, each writer buffer's ceiling is derived from the detected heap ceiling (the same 80%-of-`GOTCHA_MEM_LIMIT` ceiling described above, under "Compose-only variables") instead of the package's flat constant (256 MiB). There are six buffer "units" (events; a tracing writer's transaction buffer and span buffer count as two independent buffers of one writer; metrics; profiles; logs); the auto-default splits 60% of the heap ceiling across them, leaving 40% for everything else (HTTP ingest, JSON parsing, the PostgreSQL client, the runtime itself). On the default `docker-compose.yml` (`mem_limit: 1g`, heap ceiling ≈ 819 MiB) this works out to roughly 82 MiB per buffer — instead of a flat 256 MiB, which summed (1.5 GiB) would exceed the heap ceiling and could trigger a kernel OOM kill during a sustained ClickHouse outage. If there's nothing to derive a heap ceiling from (bare metal, no cgroup, no `GOMEMLIMIT`), behavior is unchanged: a flat 256 MiB per buffer. An explicitly set `GOTCHA_MAX_BUFFER_BYTES` always wins over the auto-default — including for constrained profiles (`docker-compose.small.yml` still sets `24 MiB` explicitly).

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
| `GOTCHA_SCRUB_KEYS` | built-in list (`password,passwd,pwd,pass,token,secret,authorization,auth,cookie,api_key,apikey,access_token,refresh_token,session,credit_card,card_number,cvv`) | Comma-separated, case-insensitive key names whose values get redacted in tags/contexts/stack traces/span data. This variable **extends** the built-in list rather than replacing it, so adding your own key (e.g. `internal_user_id`) takes one value and does not require re-listing the standard ones. To drop a specific built-in key, name it exactly in `GOTCHA_SCRUB_ALLOW_KEYS`. |
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
| `GOTCHA_RUN_EVALUATORS` | by mode | Whether to run the periodic cycles: performance regressions, metric alert rules, profile regressions, built-in host thresholds (disk/memory/load/silence), SLO burn-rate evaluation (`slo.Evaluator`), and the [escalation](/docs/escalations) scheduler (`escalation.Scheduler`) — six in total. They ship with `uptime` and `all` by default, although they have nothing to do with uptime. In a `web`+`ingest` split (no uptime in use) a metric alert rule, an SLO alert, and escalation all look enabled and **never fire** — set this on exactly one replica. A warning is logged at startup in modes without evaluators. |
| `GOTCHA_METRIC_EVAL_INTERVAL_SECONDS` | `60` | How often (seconds) metric threshold alert rules are evaluated. |
| `GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS` | `300` | How often (seconds) the profiling regression detector runs. |
| `GOTCHA_HOST_EVAL_INTERVAL_SECONDS` | `60` | How often (seconds) the background evaluator recomputes built-in host thresholds (disk/memory/load/silence) and opens/closes their incidents, see [Hosts](/docs/hosts). Minimum 1 second. Lowering it only makes sense on a small fleet: every tick queries the latest points for every host in the project. |
| `GOTCHA_SLO_EVAL_INTERVAL_SECONDS` | `120` | How often (seconds) the background evaluator recomputes SLO burn rates over the fast/slow windows and opens/closes error-budget incidents. Minimum 1 second. SLOs live on multi-day windows, so a slower tick than the metric/host evaluators is enough. |
| `GOTCHA_ESCALATION_INTERVAL_SECONDS` | `60` | How often (seconds) the escalation scheduler checks open, unacknowledged incidents and advances their [ladder](/docs/escalations) to the next step. Minimum 1 second. Gated by the same `GOTCHA_RUN_EVALUATORS` as the five cycles above. |
| `GOTCHA_DEPENDENCY_SETTLE_SECONDS` | `300` | Settling grace when a parent goes down in the [storm-suppression](/docs/alert-suppression) graph: how long to wait before notifying or escalating a dependent node that has a declared parent. |

## Observability and logs

Detail level and format of the instance's own logs.

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn` or `error`. Raise it to `debug` to get more detail during an incident without rebuilding. |
| `GOTCHA_LOG_FORMAT` | `text` | Log format: `text` or `json`. Use `json` when shipping logs to Loki/ELK. |

## Security

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_SECRET_KEY` | `insecure-dev-secret` | Signs session and OAuth state cookies, and doubles as the master key for at-rest encryption (SSO client secrets, channel tokens, monitor headers). **The default is public** (it's in the source code) — leaving it on a real server allows account takeover via OAuth login. On a non-localhost `GOTCHA_BASE_URL`, the app refuses to start in the `web`, `all`, `ingest`, and `uptime` modes (everywhere except `probe`) until a real key is set, and requires it to be **at least 32 bytes** (a shorter key is a weak signature). Generate one with `openssl rand -hex 32`. Both checks can be waived for an unusual dev setup with `GOTCHA_ALLOW_INSECURE_SECRET=1`. The value is trimmed — `"key "` and `"key"` produce the same key, so a stray space from copy-pasting doesn't change the encryption; a whitespace-only value refuses to start instead of silently falling back to the public default. See [Installation](/docs/installation), step 5, for the full walkthrough. Change the key on a running instance only via the rotation procedure — see [Privacy and 152-FZ](/docs/privacy). |
| `GOTCHA_SECRET_KEY_PREV` | *(empty)* | The previous master key, set for the duration of a `GOTCHA_SECRET_KEY` rotation — set alongside the new `GOTCHA_SECRET_KEY` during the transition, then removed. While set, the app decrypts with either key but encrypts only with the current one. The app refuses to start with a nonsensical key pair: `GOTCHA_SECRET_KEY_PREV` equal to the current `GOTCHA_SECRET_KEY`, or either one equal to the default dev key while `GOTCHA_SECRET_KEY_PREV` is set. `GOTCHA_ALLOW_INSECURE_SECRET=1` does not waive these checks — they're not about key strength, they're about a config that physically can't do what's expected of it. See [Privacy and 152-FZ](/docs/privacy) for the rotation procedure. |
| `GOTCHA_TRUSTED_PROXIES` | *(empty)* | Comma-separated CIDRs (`10.0.0.0/8`) and/or bare IPs (`192.168.1.5`, treated as `/32` or `/128`) of reverse proxies you trust. Behind a proxy (the recommended [install](/docs/installation) topology), set this to the proxy's address so the per-IP login rate limiter keys on the real client IP from `X-Forwarded-For` instead of the proxy's — otherwise every login attempt looks like it comes from the proxy and the limiter can't tell clients apart. Invalid entries are a startup error, not a silent skip. |
| `GOTCHA_ALLOW_INSECURE_SECRET` | `false` | Escape hatch that bypasses the check above — lets the app start with the default key even on a non-localhost address. **Development-only**; never set this on a real deployment. |
| `GOTCHA_REGISTRATION` | `invite` | Self-registration mode: `open` — anyone can register; `invite` — an account is created only against a valid invitation (by password or through a provider); `closed` — **no new accounts appear at all, not even with a valid invitation**. That is the difference: under `closed` an invited person hits "registration is closed", and inviting anyone from outside becomes impossible — invitations only work for people who already have an account. The very first user on a fresh instance can always register regardless of this setting (instance-admin bootstrap). |
| `GOTCHA_HSTS_ENABLED` | `true` | Whether the app itself sends `Strict-Transport-Security` on responses — and only on responses served over `https://` `GOTCHA_BASE_URL` (see [Hardening](/docs/hardening)); on a plain HTTP deploy the header is never sent regardless of this setting. Set HSTS in exactly ONE place — either the reverse proxy or the app, never both. **Turning this off does NOT un-pin browsers**: one that already received `max-age=31536000` keeps refusing plain HTTP for a year regardless — the app merely stops sending a header, it can't reach back into a browser's cache. The only way to actually un-pin is a sent `max-age=0` (see `GOTCHA_HSTS_MAX_AGE_SECONDS` below), which requires this to stay `true`. |
| `GOTCHA_HSTS_MAX_AGE_SECONDS` | `31536000` | How long (in seconds) a browser is told to remember the HTTPS requirement; the default is one year. `0` is a legitimate, deliberate value — not "off" — it's the only way to actually revoke a previously sent pin: send `GOTCHA_HSTS_ENABLED=true` with this at `0` to un-pin browsers that already cached the header, then raise it again once the emergency has passed. **If `GOTCHA_HSTS_PRELOAD=true` is still set, turn it off FIRST** (`GOTCHA_HSTS_PRELOAD=false`) — the preload check below requires at least a year of max-age, so `0` together with `PRELOAD=true` refuses to start; see [Hardening](/docs/hardening) for the full emergency-rollback order. A negative value is always a typo and refuses to start. |
| `GOTCHA_HSTS_INCLUDE_SUBDOMAINS` | `false` | Extends the HTTPS requirement to every subdomain of the host in `GOTCHA_BASE_URL`, not just that host itself. Off by default on purpose: a Gotcha instance often lives on a subdomain of a larger domain (e.g. `gotcha.example.com`), and turning this on would force HTTPS onto every *other* service on `example.com` too — services this instance has no say over and that may not even be HTTPS-ready. Only enable it if you control (or have verified HTTPS on) the whole parent domain. |
| `GOTCHA_HSTS_PRELOAD` | `false` | Marks the instance as a candidate for browser HSTS preload lists (hardcoded into browsers, bypassing the first insecure request entirely) — see [Hardening](/docs/hardening) for the actual submission step, which this variable does not perform by itself. **A one-way ticket**: once a domain is baked into a browser release, getting it removed can take months and reaches users who never even visited the instance. Requires `GOTCHA_HSTS_INCLUDE_SUBDOMAINS=true` and `GOTCHA_HSTS_MAX_AGE_SECONDS` of at least one year — the app refuses to start otherwise, because the preload list would reject a header missing either, while the operator would believe the submission had already gone through. |
| `GOTCHA_LOCALE` | `ru` | Language of outgoing notifications (uptime, regressions, performance) delivered by email/Telegram/webhook: `ru` or `en`. Recipients outside the UI have no per-user locale, so the instance operator picks one for the whole instance. Does not affect the web UI — there every user chooses their own language. |
| `GOTCHA_SSRF_ALLOW_PRIVATE` | `false` | Allow uptime checks and outbound webhook alert deliveries to target private/loopback/link-local addresses (e.g. `192.168.x.x`, `127.0.0.1`, `169.254.x.x`). Keep this `false` on any instance shared across multiple users/organizations — otherwise one user could set up an "uptime check" or webhook that actually probes your internal network (SSRF). |
| `GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME` | inherits `GOTCHA_SSRF_ALLOW_PRIVATE` | Allow uptime checks to reach private/loopback addresses. Usually the only one you need: monitoring an internal service is routine, and the target is set by an organization admin. |
| `GOTCHA_SSRF_ALLOW_PRIVATE_WEBHOOK` | inherits `GOTCHA_SSRF_ALLOW_PRIVATE` | Allow alert webhooks to reach private addresses. Riskier: up to 1 KB of the target's response is shown on the deliveries page, which turns a webhook into a reader of internal services. |
| `GOTCHA_SSRF_ALLOW_PRIVATE_OIDC` | inherits `GOTCHA_SSRF_ALLOW_PRIVATE` | Allow OIDC discovery/token calls to reach private addresses — needed for an internal IdP. Riskiest: the client secret is sent to the token endpoint taken from the discovery document. |
| `GOTCHA_SSRF_ALLOW_PRIVATE_TELEGRAM` | inherits `GOTCHA_SSRF_ALLOW_PRIVATE` | Allow Telegram Bot API calls to reach private addresses — needed only when `GOTCHA_TELEGRAM_API_BASE` points at an internal Telegram proxy/bridge. Lowest concern of the four: the base URL is set by the instance operator, not by a tenant, so it can't be turned into a user-driven SSRF the way an uptime or webhook target can. |
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
| `GOTCHA_PROBE_SERVER_URL` | *(empty)* | `--mode=probe` only: the base URL of the central Gotcha instance the probe reports to. Required in this mode, must be an absolute `http(s)` URL. |

`--mode=probe` is a separate process deployed in another region/data center: it doesn't open PostgreSQL or ClickHouse at all — it only makes outbound HTTP requests to the central instance.

## Agent distribution

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_DIST_DIR` | `/opt/gotcha/agent-dist` | Directory with `install.sh` and the built `gotcha-agent` binaries (`gotcha-agent-linux-amd64`, `gotcha-agent-linux-arm64`, `SHA256SUMS`) that the instance serves at `GET /install.sh` and `GET /agent/{file}` — the exact path the agent's install command pulls from (see [Hosts](/docs/hosts)). The default matches where the Docker build places the binaries in the image — a standard `docker-compose` deployment doesn't need to set this. This directory doesn't physically exist in dev mode (`go run` without Docker) or on a build that skipped the Docker image — both routes then answer `404` with a hint instead of failing. This variable only controls the serving directory: the `install.sh` script itself is embedded in the `gotcha` binary and is identical across every instance and product version. |
| `GOTCHA_DIST_RATE_PER_MIN` | `120` | Per-IP rate limit on `GET /agent/{file}` (agent binary and `SHA256SUMS` downloads). One install/update costs 2 requests, so the default allows ~60 hosts/minute from one IP — enough headroom for a mass rollout (Ansible/Terraform) or a fleet-wide update behind one NAT/egress address. Raise it if your fleet behind one IP is larger. `0` (or negative) removes the rate limit entirely — `GET /agent/{file}` stops being throttled, the same "0 = unbounded" convention used by `*_RETENTION_DAYS`. |

## Exports

Feature details and limits are in [Exports](/docs/exports).

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_EXPORT_DIR` | `/var/lib/gotcha/exports` | Directory on the instance's disk where the worker writes export files. In the standard `docker-compose.yml` this is the named volume `exportdata`, which survives container re-creation. If the directory can't be created on startup (no permissions, path taken by a non-directory), or it already exists but isn't writable by this process (e.g. Docker itself created the mount point for a fresh volume and it ended up owned by someone else) — the application doesn't fail: the exports section is silently disabled — the `/projects/{id}/exports` page itself still returns `200` with a note that the section is unavailable on this instance, not an empty table; only creating/downloading/deleting a job returns `404` (see [Exports](/docs/exports)). A warning is logged. The directory is created with `0700` permissions (it holds PII from events/errors, same as the files inside it, which are already `0600`): creation does not change the mode of an already-existing directory, so on installs where the directory ended up with wider permissions (e.g. 0755 from Docker mounting a fresh volume, see above), fix them by hand — `chmod 0700`. |
| `GOTCHA_EXPORT_TTL_HOURS` | `168` | How many hours after a job finishes its file is removed by the background janitor (the job is marked "expired"). `168` is seven days. The job's history row lives longer — at least 30 days from completion, see [Exports](/docs/exports) for details. Unlike `GOTCHA_DIST_RATE_PER_MIN` above, `0` or a negative value here does NOT mean "unbounded" — the file would count as expired the instant it's built. The application refuses to start with such a value. |
| `GOTCHA_EXPORT_MAX_ROWS` | `200000` | Row cap for one export: on reaching it the job is marked "truncated" (`Truncated`) and the build stops deterministically, instead of silently returning an incomplete file with no indication. `0` or negative does NOT mean "unlimited" here (not the same convention as `GOTCHA_DIST_RATE_PER_MIN`/`*_RETENTION_DAYS`) — the application refuses to start. |
| `GOTCHA_EXPORT_MAX_BYTES` | `268435456` | File size cap for one export, in bytes (256 MiB). Same idea as `GOTCHA_EXPORT_MAX_ROWS`, but by size instead of row count; same caveat about `0` and negative values. |
| `GOTCHA_EXPORT_DISK_BUDGET_BYTES` | `5368709120` | Total budget for the `GOTCHA_EXPORT_DIR` directory, in bytes (5 GiB). Checked before a file starts writing, not a partially-written file. Exceeding it is a TEMPORARY failure of the new job (up to 3 attempts, see [Exports](/docs/exports)) — it self-heals as soon as the janitor's next pass frees disk by removing expired files. `0` or negative does NOT mean "no budget" here — "used >= budget" is already true on an empty directory, so EVERY job would fail without a single attempt; the application refuses to start with such a value. |

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
