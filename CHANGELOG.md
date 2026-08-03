[English](CHANGELOG.md) · [Русский](CHANGELOG.ru.md)

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project intends to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once tagged releases begin.

## [Unreleased]

### Security
- OTLP span attributes are capped before the maps are built, not after. A 10 MiB body (about 30 KB gzipped) carries roughly 1.2 million attributes, and parsing them cost about 100 MB on top of the body — measured at 184 MB for a million attributes. The cap is 256 attributes per span; `maxDataKeys` still applies among those.
- `hostOfURL` requires an absolute http(s) URL, so a webhook target like `ftp://…` or `//evil.example/x` no longer resolves to a trusted host when deciding whether event details may be sent.
- The post-login redirect is validated where the `Location` header is set, not only where the form field is parsed: `next` was sanitised on the way in and handed to `http.Redirect` unchecked twenty lines later. Both checks now share `isLocalPath`.
- Removing a member from an organization now revokes access to their teams' projects too. Removal used to clear only the organization membership row; the matching row in `team_members` stayed behind and kept granting access to issues, traces, profiles, and ingest keys. The invariant is now enforced by the schema (migration `0029`): a team membership cannot exist without the matching organization membership. The migration deletes any dangling team memberships that had already built up.
- Password registration by invitation now requires the token from the invite link, not just a matching email. In `invite` mode, an email match against a live invitation alone used to be enough — anyone who knew the invited address could register and receive the invitation's role without proving they controlled that address.
- OTLP/JSON request bodies nested deeper than 100 levels are now rejected with `400`. The body walk was unbounded recursion, so a gzipped body of a few kilobytes could crash the whole process with a fatal stack overflow.

### Fixed
- Transaction quota is charged for what is stored, not for what arrived. Both ingest paths debited the organization for every parsed transaction and only then dropped the ones deterministic sampling had not selected, so a project sampling at 0.1 paid ten times over — and `org_usage`, the source of truth for consumption, was wrong by the same factor. With tracing disabled the envelope path charged for transactions that were never written at all. Sampled-away transactions are not counted as quota drops either: they are discarded by the project's own setting, so the drop counter now subtracts from the number selected rather than the number received.
- Quota debiting takes one round trip instead of five. The row lock that keeps two concurrent ingests of the same organization from diverging is still there — it is now taken and released inside a single statement rather than across three network pauses. Migration `0057` adds the pre-image columns this needs on PostgreSQL 17, where `RETURNING` yields only the new row; they go away when the project moves to 18.
- Deleting a project or an organization no longer runs ClickHouse mutations inside the HTTP request. The request used to issue eight synchronous mutations per project with the execution-time cap lifted, so deleting an organization with twenty projects meant up to a hundred and sixty of them in one request; a write timeout left the telemetry of the remaining projects in ClickHouse forever, unreachable — their ids were already gone from PostgreSQL. The deletion now enqueues a purge request in the same transaction that removes the row, a background worker carries it out and drops the request only after every mutation confirms, and a daily reconciliation queues telemetry of projects that no longer exist (`GOTCHA_PURGE_RECONCILE_HOURS`). Queue depth and the age of the oldest request are exported as metrics, so an unfulfilled deletion is visible.
- Copying `.env.example` to `.env` no longer breaks the very first POST. Compose reads `.env` twice — for `${…}` substitution and through `env_file` — so five uncommented variables overrode the values compose sets itself: `GOTCHA_BASE_URL` came out with port 8080 against the published 59080, and every form submit, the first-user registration included, answered `403` with nothing in the log. The five are now commented out, each with a note on when uncommenting is right, and a guard fails the build if a variable that compose sets ever comes uncommented in `.env.example` again.
- A rejected cross-origin POST is visible now. 58 of the 60 origin-check branches answered a bare text/plain `forbidden` — the operator saw a 403 on registration with a green `/readyz` and an empty log. Every branch now goes through one path that renders an error page naming `GOTCHA_BASE_URL`, writes a throttled log line with the received `Origin` (suppressed repeats are counted, not lost), and increments `gotcha_web_cross_origin_rejected_total`; a guard keeps all branches on that path. Error pages also carry an explicit `Content-Type` — they used to ship without one.
- The container healthcheck follows `GOTCHA_ADDR`. Changing the listen port used to leave the probe knocking on the dead default `:8080`, marking a healthy instance unhealthy — a second, independent symptom of the same one-line change.
- Reminder notifications carry the full monitor, `retries` included; the query built it from its own column list and silently left the field at zero.
- Migration versions are parsed with an explicit ceiling (2³¹−1), and a name without a parsable version among the embedded migrations is an error rather than a silent skip: the number lives in a platform-sized `uint` and ends up in a `bigint` column, and a silently skipped file would carry no compatibility marker — which the schema gate reads as "start forbidden".
- Regression durations were shown a thousand times too high in the regression cards, alert emails, webhooks, and Telegram: the batched endpoint p95 query returned the raw microsecond value while every reader treated it as milliseconds. The same mix-up meant the absolute floor that suppresses false alarms on small values never engaged. Migration `0030` recomputes the affected rows; web-vital metrics were already in milliseconds and are untouched.
- The notification-budget summary email printed a raw timestamp (e.g. `2026-07-31T18:00:00Z`) instead of a human one.

### Added
- `--migrate-force=N` and `--migrate-force-ch=N` clear the dirty flag an interrupted migration leaves behind. The error used to advise `migrate force N` — a binary the image does not ship. Only two targets are accepted, the current version and the previous one, because anything else is a typo that would silently shift the starting point of every future migration; the flag does not finish the migration, and says so. The startup errors now name the real command, and the upgrade docs walk through the whole situation.
- The compose stack is tunable through four substitution variables: `GOTCHA_PG_PASSWORD` and `GOTCHA_CH_PASSWORD` reach both the database container and the app DSN from one place (changing them on an existing install requires `ALTER USER` first — the procedure is in the configuration docs), `GOTCHA_PG_MEM_LIMIT` and `GOTCHA_CH_MEM_LIMIT` raise the database memory ceilings without editing the file.

### Changed
- The database containers have memory ceilings (512m for PostgreSQL, 2g for ClickHouse by default). Without a cgroup limit ClickHouse assumes 90% of the **host's** memory is its own — on the recommended 4 GB that is 3.6 GB on top of the app's gigabyte, and the night-time OOM killer picks the fattest process. Both database healthchecks get a `start_period`, so the first boot on a minimal VPS no longer fails as "dependency failed to start" while the database was ten seconds from ready. The app container is hardened: read-only filesystem, all capabilities dropped, `no-new-privileges`, a tmpfs `/tmp`, and a pids limit — the full operator path (registration, project creation, event ingest) is verified to work under all of it.
- A build without git metadata says so instead of impersonating a release. `/version` and `gotcha_build_info` carry a `stamped` field, the About page shows a note, and the version string reads "0.4.1 (no build metadata)" — an image built with bare `docker compose build` produced a string indistinguishable from a stamped release, so "deployed exactly what you think" was unverifiable. The documented build path is `make up-rebuild`, which computes and stamps the git version.
- Each summary record in PostgreSQL is now purged by the retention of the data it describes, not by `GOTCHA_RETENTION_DAYS` alone. Metric incidents follow `GOTCHA_METRIC_RETENTION_DAYS`, profile regressions follow `GOTCHA_PROFILE_RETENTION_DAYS`, and resolved uptime incidents get their own `GOTCHA_INCIDENT_RETENTION_DAYS` (90 days by default) because they have no telemetry of their own while the public status page promises ninety days of history. **Resolved profile regressions are affected visibly:** with the defaults they are now removed after 7 days instead of 90, and on an instance with `GOTCHA_PROFILE_RETENTION_DAYS=1` after a day. A regression card whose samples are already gone shows nothing, which is why they should not outlive them; raise `GOTCHA_PROFILE_RETENTION_DAYS` if you need them kept longer.

### Documentation
- OTLP trace ingest (`POST /v1/traces`) is documented: it was fully implemented while the SDK page listed protocols exhaustively without naming it, so a team on OpenTelemetry would read that traces require a Sentry SDK.
- Headings carry anchors, and the anchors are transliterated rather than numbered — a link to a section no longer breaks when a paragraph is inserted above it.
- The difference between `invite` and `closed` registration is stated: under `closed` no account is created even against a valid invitation.
- The probe run command no longer references a non-existent `gotcha` image, in the UI as well as in the docs.
- Corrected: what `GOTCHA_SCRUB_ALLOW_KEYS=email` actually does, the full list of IP field names, the note that allow-keys is checked before the email/IP rules, the backup instructions (materialized views must not be restored), the `/healthz` and `/readyz` split, the evaluator interval being a default rather than a constant, and a field name in the uptime docs.
- `.env.example` is monolingual again and the scrubbing comment sits with its variables; `configuration.md` gained "Limits and evaluators" and "Observability and logs" sections so variables live where they belong.
- The installation steps set the secret key and `GOTCHA_BASE_URL` **before** creating the first user: registration is a POST, and with a wrong `BASE_URL` that very first POST answered 403 — the explanation lived in Troubleshooting instead of the path itself. The `.env` file is created with `chmod 600` in the same command: it holds the master key that encrypts channel and SSO secrets, and the backup docs already required 600 for its copy.
- What an `unhealthy` container does and does not get: `docker compose` does not restart one — the label in `docker ps` changes and nothing else (reacting to unhealthy is Swarm/Kubernetes). How to watch it from the outside (an uptime monitor on `/readyz` from another instance — exactly what the product does), and why there is deliberately no auto-healer holding the Docker socket: socket access is root on the host. The compose comment about the healthcheck now says the same instead of implying a restart.
- The `/version` field list names `stamped` and `go` — it claimed to be complete without the latter.

### Testing
- Route tests check that the route is actually registered: 43 of them asserted a 404 that an unregistered path returns just as well, so deleting any registration kept them green.
- A guard now fails when a key used in code is missing from both catalogs — a page rendering raw keys instead of column headers used to pass the whole suite. Dynamically built keys (issue levels, probe statuses, range presets, help sections, monitor error codes) are pinned by tests that read their value sets from the code.
- `internal/auth`, `internal/org`, and `cmd/gotcha` have coverage floors; `cmd/*` is measured as its own group rather than diluting the backend number. Thresholds can no longer be lowered by an environment variable — the ratchet is enforced, not agreed upon.
- Reusable test containers carry the image tag in their name: bumping the PostgreSQL version silently reused the old container, so the whole suite validated a different engine.
- Template tests assert column order, not just that a substituted value appears somewhere.
- The Cyrillic-literal guard matches «ё» and no longer treats `catalog.` or `dialog.` as a logging call.
- Guards that used to check a hand-picked list of known cases now walk the whole tree and check everything, with an explicit, capped exception list for anything not fixed yet. This catches, wherever it occurs rather than only where the old guard happened to look: a raw i18n key reaching a page in either language, an interactive control left on the decorative border instead of the contrast-checked one, a class referenced in markup but never defined in CSS, light- and dark-theme rule pairs drifting out of sync, a destructive SQL form the migration-safety check doesn't recognize, a mutating route with no `Origin` check, and a coverage floor lowered through an environment variable.
- A guard now fails on time or duration formatted outside `internal/humanize` — a literal or variable-held `.Format(...)`, `.Sub(...).String()`, or a literal `Duration.String()` — walking the whole tree with a capped, per-line exception list rather than a hand-picked set of known cases. It caught a place no past audit had: the notification-budget summary email.
- CI builds the Docker image the way the docs tell the operator to (`make build`, with full git history so the version gets stamped), validates both compose files, and boots the whole stack, waiting on every healthcheck and then asking `/readyz` — none of the compose file, the Dockerfile, or the healthchecks were exercised by CI before. `govulncheck` and a `go mod tidy` drift check gate every push. New guards parse the compose files: every service must carry a restart policy, log rotation, a memory ceiling, and a healthcheck with `start_period` — the class of defect where a service is added without its limits, which is exactly how the unbounded ClickHouse memory and the missing `start_period` appeared.

### Accessibility
- Interactive controls use the contrast-checked border token again. Issue filters, monitor-type tabs, chips, and the language/theme switches had it overridden back to the decorative one — measured 1.40:1 in the dark theme against the 3:1 required by WCAG 1.4.11.
- Muted text meets 4.5:1 in both themes: "Unassigned" and the weekday headers in the date picker were at 2.70–3.12:1.
- Error and notice banners take the ink tokens (4.15:1 and 3.65:1 before), user initials and the quota-banner link take a new `--accent-ink` (4.05:1 and 4.45:1 on their pale backgrounds).
- The auto-dismissing flash leaves the accessibility tree instead of only fading out — keyboard focus used to land on an invisible close button — and its timer pauses while hovered or focused (WCAG 2.2.1).
- Language and theme switches, and row checkboxes, meet the 24×24 target size (WCAG 2.5.8); they were 33×22, 30×22, and 13×13.
- The assignee select and the four role selects have accessible names; focus is visible after the skip link; duplicate element ids are gone.
- Chart colours come from theme tokens rather than a hard-coded brand hex, and gradient ids are unique per document.

### Interface
- Pending invitations are listed on the organization settings page and can be revoked. Mistype an address and the link went to a stranger — with no way to see it or take it back, though the service could already do it.
- The issue list uses the same time-range control as every other page, including the hour preset and custom ranges, plus an "all time" option. A window picked on Performance no longer resets when you open Issues.
- Monitor validation errors are shown in the interface's language and next to the reason, instead of "монитор: uptime: invalid monitor: http url must be a valid http(s) URL".
- One-off maintenance windows are shown in their own timezone; incident durations and delivery timestamps are shown in words instead of "23m0s" and "2026-07-29T08:22:27Z".
- A relative timestamp ("5 hours ago") now shows the exact moment too, in all nineteen places it appears: hover it for the precise time, or read the `datetime` attribute for tooling.
- A custom date range typed by hand ("01.02") used to render as `DD.MM`, indistinguishable in either language from January the 2nd; it now renders as `YYYY-MM-DD HH:MM ZONE`, unambiguous and consistent with every other absolute timestamp in the interface.
- A metric alert rule's window showed raw seconds ("3600s", "86400s"); it now reads as a duration ("1 hour", "1 day").
- Monitor type, project platform, metric type, and rule aggregation are shown as human labels instead of their raw programmatic names, in both languages.
- A closed incident's duration on the public status page — visible to the instance owner's own customers, not just staff — is shown in words in the visitor's language, instead of the raw duration baked into the cached page.
- Reissuing a heartbeat token now asks for confirmation: it breaks a working cron job and cannot be undone.
- Consensus options, the search placeholder, the empty-issues title, the heartbeat grace period, and the alert throttle field are no longer English fragments in the Russian interface. Uptime columns no longer carry two names for the same thing.
- Web Vitals columns state their units in the header rather than only in a hover tooltip.
- The "Change assignee" control no longer looks disabled; modals close with Escape; soft hyphens no longer break Ctrl+F in the navigation rail; the quota banner breaks the rejected count down by kind; the incidents and teams pages gained the "What is this section?" panel.

### Security
- `POST /onboarding` now applies the same rule as `GET`: onboarding is for someone who has no project yet. Only the form was gated, so an invited member could create organizations, projects, and ingest keys in a loop.
- Ingest and service routes (`/healthz`, `/readyz`, `/version`, `/metrics`) answer with `X-Content-Type-Options: nosniff` and `X-Frame-Options: DENY`. They are registered on the root mux and by Go 1.22 routing precedence they shadow `/`, so they bypassed the security-header middleware entirely — while ingest also sends `Access-Control-Allow-Origin: *`.
- A monitor can only be assigned to a region the organization actually has. The form offered only valid regions, but the POST accepted any string, and a monitor in a region nobody leases is never checked — which looks like "no failures" rather than "no monitoring".
- The "back" breadcrumb rejects protocol-relative referers in the `/\` form, matching the three sibling functions that already did.

### Performance
- The spike detector now asks ClickHouse once per rule instead of once per active issue. A project with 10,000 active issues cost 10,000 round trips every minute — and the number of issues is set by whoever sends events, through unique fingerprints.
- Regression evaluators batch their queries: profiles go from 1 + 2K queries per service to two, and traces from over two hundred per project to six. The profile evaluator also stopped walking every project in the installation — it now works from the services that actually have data.
- The cardinality guard caps the number of distinct field NAMES per project (200) and the total number of remembered values (about a million, `gotcha_cardinality_tracked_values`). Field names come from the sender just like values do, so "one field per event" bypassed the guard entirely.

### Fixed
- A public status page never shows more history than is actually retained: the window is capped by `GOTCHA_RETENTION_DAYS`. Shortening retention used to leave the page drawing 90 cells, most of them empty, which reads as a monitoring failure rather than a retention setting.
- Changing retention now logs a warning naming the old and new values. TTL is a property of the installation but is configured per replica, so replicas that disagree flip it back and forth — and every flip rewrites every part of the table.

### Changed
- `/healthz` now answers 200 whenever the process serves HTTP, and no longer returns 503 when a database is unreachable. Readiness moved to the new `/readyz`. Pointing a liveness probe at the old behaviour restarted a healthy container on every storage outage, and each restart threw away the buffers that were waiting for storage to come back. Update anything that watches `/healthz` for readiness.
- Notifications are delivered concurrently (`GOTCHA_NOTIFY_CONCURRENCY`, default 4) and the worker keeps claiming batches while the queue is not empty. One dead webhook holding a 30-second timeout no longer delays every other notification.
- `gotcha_pipeline_dropped_tasks_total` gained a `reason` label: `queue_full`, `queue_bytes`, `storage_error`, `panic`, `closed`.

### Added
- Events dropped because PostgreSQL was unavailable are now counted (`reason="storage_error"`). They were only logged, so the documented rule "no drops means events never arrived" sent operators to check their SDK during a database outage.
- The heap ceiling is derived from the container's memory limit at startup, in any deployment that sets one — compose, Kubernetes, systemd. Go does not read cgroup limits, so buffers used to grow past the limit until the kernel's OOM killer discarded all of them. An explicit `GOMEMLIMIT` still wins. Reported as `gotcha_memory_limit_bytes`.
- `docker-compose.yml` sets `mem_limit` (1 GB by default, `GOTCHA_MEM_LIMIT` to change) and a container healthcheck; the image gained `HEALTHCHECK`. A hung process used to sit `Up` forever, and there was no readiness signal during first-boot migrations.
- Delivery metrics: `gotcha_notify_sent_total`, `_failed_total`, `_retried_total`, `gotcha_notify_pending_jobs`, `_failed_jobs`, and `gotcha_notify_oldest_pending_age_seconds` — the last one is what tells an idle queue from a stuck one.

### Fixed
- Shutting the process down no longer leaves up to five notifications waiting for the claim lease: the result of an attempt that already happened is written regardless of cancellation.
- The next retry time for a notification is computed by the database clock rather than the process clock.

### Fixed
- Claim windows for the notification outbox, alert throttling, and the alert budget are now computed by the database clock. A process whose clock ran fast could take another worker's lease and send an alert twice.
- Weekly maintenance windows keep the duration you configured across daylight-saving transitions. A 02:00–04:00 window collapsed to one hour on the spring transition and started an hour late on the autumn one, missing the first check.

### Added
- Retention now covers summary records in PostgreSQL: issues and performance issues whose last event is older than `GOTCHA_RETENTION_DAYS`, and closed incidents and regressions. Open incidents are never deleted. Deletions are counted by `gotcha_entities_purged_total`.
- Schema versions carry a backward-compatibility marker, recorded in the database as migrations are applied. An older binary can now start against a newer schema when every migration it is missing is additive — rolling a release back no longer requires restoring from a backup. See Upgrading.

## [0.4.1] - 2026-07-27

### Documentation
- Docs pages for the time-range control and for reading a performance issue.

## [0.4.0] - 2026-07-27

### Added

- **One time-range control on every chart page.** Issues, performance and
  endpoint views, Web Vitals, metrics, profiles and the monitor latency chart
  now share a single time-window control — presets (1h / 24h / 7d / 30d) plus a
  custom range — where each page used to have its own fixed or ad-hoc window.
  The issue frequency and monitor latency charts, which had no window control at
  all, gain one.
- **Single-popup date-range picker.** Choosing a custom range opens one popup
  with a two-month calendar and the quick presets: click the start, click the
  end. It is a progressive enhancement — with JavaScript off the control falls
  back to the preset dropdown and two native date fields, still fully working.
- **Performance issues now explain the problem.** A perf-issue page opens with a
  plain-language "What's happening / How to fix" for its kind (N+1, slow query,
  HTTP flood), shows the full, un-truncated query from the offending span (with
  its operation, database and duration), and — when the SDK reports it — the
  code location (file, line, function) behind the query.
- **Tooltips that explain the numbers.** Hover any endpoint metric (p50 / p75 /
  p95 / p99, failure rate, Apdex, throughput) on the endpoint list and detail to
  see what it means; Web Vitals (LCP / INP / CLS / FCP / TTFB) and the
  performance and regression settings fields get the same hints.
- **Full request details on issues.** The request that triggered an error —
  method, URL, query parameters, body and headers — is captured (scrubbed for
  PII) and shown on the issue page.
- **"Back where you came from" navigation.** The back-link on a detail page now
  returns to the page you actually arrived from, not a fixed parent.
- **Pagination** on the incidents list and on a monitor's incident feed.
- The **issues list defaults to the "unresolved" filter**, still overridable.

### Changed

- **Charts span the selected window.** A 30-day window now draws all 30 days,
  with gaps where there is no data, instead of squeezing the axis to whatever
  range happens to contain data (Grafana/Elasticsearch behaviour); time labels
  thin adaptively so a wide window no longer overlaps them into an unreadable
  strip.
- Clicking a project name in the projects table opens its settings.
- A pass of UI-consistency polish: a distinct colour for "partial outage",
  chart x-axis and flamegraph fixes, equal-height alert-rule cards, a responsive
  docs index, and a centred status-page settings column.

### Fixed

- **Dark-theme contrast.** The time-range preset dropdown rendered white-on-white
  and the trace-waterfall span labels rendered black-on-dark — both now follow
  the theme, and native date pickers render in the active theme.
- Spacing above the quota and SSO save buttons in organisation settings.

## [0.3.2] - 2026-07-25

### Added

- **Per-check retries** for uptime monitors: a monitor can retry a failed check
  up to N times (0–10, 1s apart) before recording it, absorbing transient blips
  — such as a periodic front-side TLS-handshake tarpit that passes on an
  immediate retry — without raising false incidents. Orthogonal to the fail
  threshold (which counts already-recorded failures). Set it when creating or
  editing a monitor.

## [0.3.1] - 2026-07-25

## [0.3.0] - 2026-07-24

### Added

- **Low-resource deployment overlay** (`docker-compose.small.yml`) for minimal
  servers (2 vCPU / 2 GB): caps ClickHouse memory and event ingestion to fit
  constrained hardware. On a 2-core / 2 GB VPS it cut ClickHouse memory
  880 → 295 MB, disk 12 → 9.2 GB and load average 1.15 → 0.79. Apply it only on
  weak hardware — on a server with headroom it would throttle ingestion.

### Fixed

- **Uptime latency chart** now renders with axes, a millisecond scale, time
  labels, a phase legend (DNS/TCP/TLS/TTFB) and per-hour tooltips. Its scale
  is set by the drawn phase timings rather than the average total, so a single
  request timeout no longer flattens healthy bars into an unreadable row of
  stubs; hours above the scale are flagged with a marker whose tooltip shows
  the real total.
- **Availability bar** gained a distinct "partial" (amber) state: a monitor
  that is mostly healthy but has occasional failed checks no longer shows as
  fully red, so a ~90 %-up monitor reads as "occasional blips", not "down".
- Hardened integer parsing in configuration and migration-version handling
  (bounded conversions), clearing CodeQL incorrect-conversion warnings. No
  behaviour change — inputs are trusted operator env config and embedded
  migration filenames.
- ClickHouse system logs now carry a retention TTL and the PostgreSQL planner
  assumes SSD storage — universal defaults added to the base compose file;
  previously ClickHouse system-log disk usage grew without bound on any server.

## [0.2.1] - 2026-07-23

## [0.2.0] - 2026-07-23

## [0.1.0] - 2026-07-22

Initial feature set of the self-hosted release:

### Added

- **Issues / error tracking**: Sentry-SDK-compatible event ingestion,
  automatic grouping into issues, stack traces, breadcrumbs, tags/contexts.
- **Performance / tracing**: distributed traces and transactions, Web
  Vitals, performance-issue detection, and regression detection.
- **Metrics**: OTLP metrics ingestion, metric queries, threshold-based
  alert rules and incidents.
- **Profiling**: CPU/flamegraph profiles from Sentry profiling payloads and
  pprof, with profile regression detection.
- **Uptime monitoring**: HTTP checks from a built-in local region or remote
  probes (`--mode=probe`), incident detection, public status pages.
- **Alerting**: delivery via email, webhook, and Telegram; rules for new
  issues, spikes, metric thresholds, and performance/uptime regressions.
- **Organizations, teams and RBAC**: multi-tenant organizations, projects,
  and membership roles.
- **SSO**: generic OIDC, Yandex ID, and VK ID login, each independently
  configurable.
- **Privacy and safety defaults**: server-side PII scrubbing (IP/email
  zeroing, key-based redaction) and SSRF protection for outbound
  webhook/uptime requests, both on by default.
- **Configurable retention** per signal type (events/transactions, spans,
  metrics, profiles) via ClickHouse TTLs.
- **Per-project performance settings** and span retention overrides.
- Single-binary deployment (`gotcha`) with `--mode=ingest|web|uptime|probe|all`.
- In-product documentation (`/docs`) in English and Russian.
- Open-source project files: README, LICENSE (Apache-2.0), CONTRIBUTING,
  SECURITY, CODE_OF_CONDUCT, `.env.example`.
- Build version surfaced in the UI footer, an About page, `--version`, and
  `/version`.
