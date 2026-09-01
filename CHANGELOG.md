[English](CHANGELOG.md) · [Русский](CHANGELOG.ru.md)

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- New "Overview" screen for a project (`/projects/{id}/overview`): what
  needs attention right now — recent errors, active alerts, deploy markers —
  instead of dropping straight into the issue list.
- New "All projects" screen for an organization (`/orgs/{id}/projects`): a
  flat list of every project in the organization, reachable from an
  organization switcher rather than nested one level inside a project.
- The sidebar is now split into three tiers: work areas (Overview, Issues,
  Performance, Logs, Metrics, Hosts, Uptime), setup (Alerts), and a footer
  (Settings, Documentation). The context column groups related pages under
  each area, and the top bar now shows where you are: organization, project,
  page.
- New index supporting the "new since" error counter on the Overview screen;
  the migration applies automatically on the next start.

### Changed
- Several sections were renamed to describe what's actually inside them
  instead of repeating their parent's name: "Transactions" is now
  "Performance"; under the Issues area, the subsection that used to just
  repeat the area's own name is now called "Errors"; "Performance issues"
  is now "Bottlenecks"; uptime "Incidents" is now "Availability incidents";
  the alerts subsections are now "By errors" and "By metrics"; the
  "Organization" area is gone, folded into the project/organization
  switcher and settings; "Projects" is now the "All projects" entry in that
  switcher.
- No page changed address: every screen kept its URL, so old bookmarks and
  links keep working. Two exceptions changed behavior rather than address:
  `/projects` now redirects to your organization's project list instead of
  showing its own page, and a project's old incident-feed link now
  redirects to its Overview.

## [0.28.0] - 2026-09-01

### Added
- Strict-Transport-Security is now configurable: `GOTCHA_HSTS_ENABLED`,
  `GOTCHA_HSTS_MAX_AGE_SECONDS`, `GOTCHA_HSTS_INCLUDE_SUBDOMAINS` and
  `GOTCHA_HSTS_PRELOAD`. Defaults keep today's behaviour (one year, no
  subdomains, no preload); set `GOTCHA_HSTS_ENABLED=false` when the reverse
  proxy already sends the header. See /docs/hardening.
- New documentation page "Hardening your install": what the reverse proxy
  closes and what the app closes, which service endpoints to keep off the
  public internet, TLS/HSTS, security.txt and a curl self-check.

## [0.27.1] - 2026-09-01

### Fixed
- Project settings: a key's DSN is shown on its own full-width line under the
  key instead of a narrow column, so the address no longer wraps one character
  per line and the revoke button no longer slides under a horizontal scrollbar.

## [0.27.0] - 2026-09-01

### Added
- Ingest DSN keys now have a type: `browser`, `server`, `agent`. A key is
  granted access only to what its source actually needs; keys issued before
  this release keep working unchanged and are marked as untyped in the
  project settings. See /docs/keys.
- `gotcha_host_registrations_scope_skipped_total` counter, and a `key_scope`/
  `scope` value on the ingest rejection metrics.

### Changed
- A new project now gets three keys right away — one per source class —
  instead of a single shared one.
- A host now registers only through a key of type `agent`. Metrics exports
  with other key types are still accepted, but no longer register a host.
- The single "project DSN" field in project settings is gone; each key's DSN
  now shows in its own row of the keys table.

## [0.26.0] - 2026-08-31

### Added
- Ingest endpoints now live in gotcha's own `/api/v1/*` namespace:
  `POST /api/v1/logs` (NDJSON logs), `POST /api/v1/profiles/pprof` (pprof
  profiles) and `POST /api/v1/{project}/deployments` (deploy markers). The
  deploy path accepts both forms, with and without a trailing slash. The OTLP
  endpoints (`/v1/traces`, `/v1/metrics`, `/v1/logs`) and the Sentry-compatible
  ones (`/api/{project}/envelope|store`) are unchanged — their paths belong to
  those standards, not to us.
- `gotcha_ingest_deprecated_path_total{path="…"}` self-metric, counting requests
  that still arrive on a deprecated ingest path. **Temporary by design:** it is
  removed in 1.0 together with the deprecated paths, so do not build long-lived
  dashboards on it. See /docs/self-monitoring.

### Deprecated
- `POST /logs`, `POST /profiles/pprof` and `POST /api/{project}/deployments/`
  are deprecated in favour of their `/api/v1/*` equivalents above. They keep
  working exactly as before — same authentication, rate limiting and quotas —
  and responses now carry `Deprecation` and `Link; rel="deprecation"` headers.
  **They will be removed in 1.0.** Point your senders at the new paths; the
  `gotcha_ingest_deprecated_path_total` counter above tells you whether anything
  still uses the old ones.

## [0.25.0] - 2026-08-31

### Added
- `GOTCHA_SECRET_KEY_PREV` environment variable to support rotating
  `GOTCHA_SECRET_KEY` without losing access to previously encrypted secrets:
  set it to the old key alongside the new `GOTCHA_SECRET_KEY`, restart, and
  the app re-encrypts everything readable (SSO client secrets, channel
  tokens, monitor headers) under the new key. See /docs/privacy for the full
  procedure.

### Changed
- At-rest encrypted values now carry a format version and the ID of the key
  they were sealed with (`enc:v2:<key-id>:...`), so key rotation can be
  verified directly in the database instead of by trial and error. The
  previous format (`enc:...`, no version) is still read without limit.
- Encryption backfill now runs on every start and upgrades everything
  readable to the new format under the current key, including values that
  were never encrypted because the instance once ran on the default dev key.
  Previously, setting a real key after the fact left existing rows in plain
  text until they were manually re-saved.

### Breaking
- **Downgrading to a version below this one is not supported once any
  secret has been written in the new envelope format.** An older binary
  doesn't recognize `enc:v2:...` as ciphertext, treats it as a legacy plain
  secret, and sends it out as-is — delivery channel and SSO integrations
  will silently break. Roll forward only.
- **During a rolling deploy, keep secret key rotation off until every
  instance is on this version.** An old binary running alongside a new one
  doesn't understand `enc:v2:...` either: it hands the value back as-is as
  if it were live, and writes fresh secrets in the old format, bypassing the
  backfill the new binary already ran.

## [0.24.0] - 2026-08-28

### Added
- New self-metrics: `gotcha_ingest_rejected_total{reason,signal}` — ingest
  rejections by reason (rate limit, quota, body size, malformed body) and
  telemetry signal, `gotcha_i18n_missing_key_total{locale,stage}` and
  `gotcha_uptime_heartbeat_ignored_total{reason}`.
- The public status page now explains itself to an outside reader: the time zone
  of the timestamps is stated and the meaning of the "Paused" status is spelled
  out.

### Changed
- **Breaking.** Ten environment variables have been renamed: the unit of
  measurement is now part of the name, and the agent-distribution and remote-probe
  variables carry the correct subsystem prefix. The old names are no longer read —
  update your `.env` and compose variables before upgrading. An old name set to a
  non-empty value now refuses to start, naming the new variable, instead of
  silently applying a default.

  | Before | After |
  |---|---|
  | `GOTCHA_METRIC_EVAL_INTERVAL` | `GOTCHA_METRIC_EVAL_INTERVAL_SECONDS` |
  | `GOTCHA_PROFILE_EVAL_INTERVAL` | `GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS` |
  | `GOTCHA_HOST_EVAL_INTERVAL` | `GOTCHA_HOST_EVAL_INTERVAL_SECONDS` |
  | `GOTCHA_SLO_EVAL_INTERVAL` | `GOTCHA_SLO_EVAL_INTERVAL_SECONDS` |
  | `GOTCHA_ESCALATION_INTERVAL` | `GOTCHA_ESCALATION_INTERVAL_SECONDS` |
  | `GOTCHA_RETENTION_DAYS` | `GOTCHA_EVENT_RETENTION_DAYS` |
  | `GOTCHA_SERVER_URL` | `GOTCHA_PROBE_SERVER_URL` |
  | `GOTCHA_INGEST_RATE_LIMIT` | `GOTCHA_INGEST_RATE_PER_SEC` |
  | `GOTCHA_AGENT_DIST_DIR` | `GOTCHA_DIST_DIR` |
  | `GOTCHA_AGENT_DIST_RATE_PER_MIN` | `GOTCHA_DIST_RATE_PER_MIN` |

- **Breaking.** The queue self-metrics now follow a single naming canon,
  `gotcha_<subsystem>_queue_*`: `gotcha_export_pending_jobs` →
  `gotcha_export_queue_depth`, `gotcha_export_oldest_pending_age_seconds` →
  `gotcha_export_queue_oldest_seconds`, `gotcha_export_failed_jobs` →
  `gotcha_export_queue_failed`, likewise for `gotcha_notify_*`;
  `gotcha_pipeline_queued_tasks` → `gotcha_pipeline_queue_depth`,
  `gotcha_pipeline_queued_bytes` → `gotcha_pipeline_queue_bytes`. Update your
  dashboards and alerts.
- Export metadata (the group id, the filter code, the pseudonymisation notice) has
  been taken out of the file bodies and is delivered alongside them instead: a
  `?meta=1` response on the download, attributes on the Exports page and a line in
  the "export is ready" email. CSV/JSON/NDJSON files can again be read with
  standard tools, with no special handling of the first line or the first element.
- The recommended heartbeat command is now `curl -fsS -X POST`; `GET` remains
  supported.

### Fixed
- The remote-probe launch command shown in the interface referred to an
  environment variable that no longer existed: a probe started by copying that
  command would not have come up.
- A heartbeat is no longer credited for requests marked as a prefetch or a link
  preview (messenger bots, antivirus proxies, browser prefetch): previously a link
  forwarded into a chat could silence a real alert.
- Posting a deploy marker with a body over the limit now answers `413` instead of
  `400` "malformed JSON", in line with the other ingest endpoints.
- The public status page requested its stylesheet without a version, promising
  caching proxies a different policy than the rest of the interface.
- The export form now validates the status and the level against a closed set:
  previously an arbitrary string was stored in the job and displayed on the list
  page. Form errors now name the reason instead of a generic "Action failed".
- `GOTCHA_DEPLOY_RETENTION_DAYS` rejects a negative value at startup — previously
  it silently disabled the deploy-marker cleanup.
- A missing translation key is now observable: a log warning (at most once per
  minute per key) and a metric. Rendering behaviour is unchanged.
- The `host_incidents.current_value` and `peak_value` columns are now `NOT NULL` —
  the schema no longer allowed a state on which reading an incident failed.

## [0.23.0] - 2026-08-27

### Added
- The log quota is now configurable in organisation settings alongside the other
  quotas. The documentation described this setting, but the form had no row for
  it: the quota was set once at organisation creation and could not be raised for
  an existing organisation afterwards.
- Notifications now name the project — in the email subject, in the body and in
  the webhook payload. Previously the project was identified only by a numeric
  id, which made an alert unrecognisable when one channel served several
  projects.
- New metrics in `/metrics`: `gotcha_ingest_key_rejections_total{reason}`, plus
  liveness and tick-duration metrics for the metric, trace and profile
  evaluators, the escalation scheduler and the uptime monitoring loops.

### Changed
- The constrained profile (`docker-compose.small.yml`) no longer pins `GOMEMLIMIT`.
  The application derives its heap ceiling from the container's cgroup limit on its
  own (80% of `mem_limit`), and the pinned value only overrode that mechanism —
  it predates it. The effective ceiling goes from 200 MiB to 204.8 MiB.
- `docker-compose.yml` no longer publishes the application port on every network
  interface: by default it listens on `127.0.0.1` only. Without this, plain HTTP
  and an open `/metrics` (unauthenticated telemetry) stayed reachable from
  outside, bypassing the HTTPS proxy. **Who is affected:** installations with no
  reverse proxy that opened the interface directly at the server's address — that
  access disappears. **What to do:** the recommended path is a reverse proxy on
  the same host — a loopback bind is enough for it, and
  `proxy_pass http://127.0.0.1:59080` keeps working unchanged; the quick path is
  to add `GOTCHA_BIND=0.0.0.0` to `.env`, which restores the previous behaviour,
  remembering that plain HTTP and `/metrics` then face outwards again.
- Uptime recovery notifications now go only to the channels that received the
  alert for that incident, matching every other incident source. Previously they
  went to every channel deliverable to the monitor or the project at closing
  time, so a channel added after an incident had started could see "incident
  closed" without ever having seen "incident opened".

### Fixed
- Deleting a project left its logs behind in ClickHouse indefinitely (until they
  aged out on their own retention schedule), even though the interface reported
  the deletion as successful.
- Deleting or exporting an end user's personal data (right of access/erasure) did
  not cover logs.
- The backup instructions did not include the logs table. Restoring from a backup
  by the book silently lost the entire log history, with no error to notice it by.
- The issue export could silently skip active groups: it paged through results
  ordered by "last seen", a column that keeps changing while the export is
  still running, so a group that received a new event mid-export could jump
  ahead of the page already read and never get read at all — while the export
  still reported itself complete. The export now takes a fixed snapshot of
  which groups match the filter, in the same "most recently active" order,
  before it starts reading rows, so a group's activity changing mid-export can
  no longer make it disappear. A group created after the export started is not
  included, by design — the file reflects the state at the moment the request
  was made, not a moving target. Row order in the file is unchanged.
- Submitting the "Exports" page's own request form (the one with no period
  selector of its own) silently inherited whatever time window was last set on
  an unrelated screen (logs/hosts/metrics/issues), through a cookie those pages
  share — instead of the "all time" default the page claimed. It now always
  defaults to "all time" unless a period is passed explicitly, and the period a
  request was made for is now shown in the request list.
- Retrying a bulk issue action or a subject data erasure request that ended up
  affecting nothing (e.g. the selected issues were already in the target
  status, or a repeated erasure request) showed the raw, untranslated message
  key (e.g. `flash.subject_purged`) instead of a readable confirmation.
- A monitor's "down" notification that failed to send (a dead email/webhook
  channel) had no real retry: the only retry path was tied to a 5-minute
  dependency-suppression grace period, so an outage shorter than that got no
  notification at all, ever — neither the "down" nor, since it was never sent,
  the eventual "up". A failed delivery is now retried on the very next check,
  independent of that grace period, up to 5 attempts. If the channel is truly
  dead and every attempt is exhausted, the incident now carries a visible
  "notification not delivered" badge in the interface — previously it looked
  exactly like any other open, unacknowledged incident, so a permanently
  broken channel meant nobody found out even by looking.
- Uptime monitors were the only incident source not covered by escalation
  policies or acknowledgment: a site going down could not be escalated to
  further channels over time, and there was no way to acknowledge it to stop
  paging. Both now work the same as for hosts, metrics, traces, profiles, and
  SLOs.
- Escalation-step logging and level advancement could disagree if the process
  died between the two, in either direction (a duplicate page on the next
  attempt, or a silent gap in the escalation log with the level advanced past
  it). Logging is now safe to retry, and a logging failure no longer lets the
  level advance past it.
- Who acknowledged an incident was recorded but never shown anywhere in the
  interface.
- The escalation ladder editor silently dropped a channel from a step the next
  time the form was saved if that channel had stopped being deliverable in the
  meantime (e.g. a webhook's secret broke), with no warning at all. It now
  stays visible in the form, marked as undeliverable and why, and survives
  being saved as-is.
- On the dependency map, an edge label slid under the node rectangle: the text
  was drawn with no anchor and was then painted over by the node drawn next. It
  hit the busiest dependency deterministically.
- The Y-axis label on charts was clipped by the left edge of the image. It is now
  pinned so that it neither runs past the boundary nor intrudes into the plot
  area; with a very long label in a narrow field the plot area wins and the label
  is clipped on the left rather than overlapping the chart.
- The first and second X-axis labels could overlap: switching the anchor on the
  outermost tick shifted that label, and tick spacing did not account for it.
- Date labels on the event-frequency chart could overlap when the observation
  window began shortly before UTC midnight (the "24 hours" preset). The second
  label is now dropped entirely in that case — one date on the axis is better
  than two unreadable ones on top of each other.
- The availability bar on the public status page was drawn at its own 192 pixels
  and centred in the card instead of filling its width.
- The page title in the top bar was cut off hard, with no ellipsis — every title
  without exception on a 360px-wide screen.
- The event-frequency chart on the issue card was an unreadable smudge at 360px:
  the promised horizontal scrolling never engaged and the axis labels piled up on
  top of each other.
- Event context cards pushed the page wider than a phone screen, taking the whole
  page sideways with them.
- The public status page — the only page outsiders see — had no protection
  against wide content: a long monitor name (an FQDN) stretched the incident and
  maintenance-window tables, and the page with them.
- The code sample on the SDK setup page — the first screen a new user sees —
  wrapped in the middle of a token instead of scrolling horizontally.
- Expanding the export form on an issue card tore the page apart: the form stood
  in the flow behind the last button of its row, leaving a hole and pushing
  everything below it down.
- A failed ClickHouse migration alongside a successful PostgreSQL one no longer
  makes rolling the version back impossible. PostgreSQL compatibility markers are
  now written right after its own migration rather than in a common tail after
  both schemas — previously this scenario required restoring the database from a
  backup, even though the PostgreSQL changes themselves were safely additive.
- Ingest rejections caused by a wrong or missing key no longer pass without a
  trace: they are counted in a metric labelled by reason and written to the log.
  An instance could previously turn away thousands of events while the interface
  showed nothing wrong.
- A trailing slash in `GOTCHA_BASE_URL` no longer ends up in generated links
  (invitations, the OAuth return address, the uptime check command).
- Stale-data cleanup now also runs at startup, not only an hour in: with frequent
  restarts, exports, delivery logs, sessions and escalation history were never
  cleaned up at all and the disk budget was never released.
- The PostgreSQL connection pool now has explicit bounds (connection count and a
  statement timeout) — it had none at all.
- The startup warning about running without background evaluators named four
  loops out of six: the SLO check and the escalation scheduler went unmentioned,
  although the flag gates them too.
- Issue alerts were assembled in two different locales at once: the subject and
  the body were built in the instance locale, while the project name and the
  anonymised text for external channels were built in the default one.
- For uptime monitoring, a failure to record which channels had received the
  alert (escalation step 0) was not retried: a channel that genuinely got the
  alert could permanently lose its recovery notification because of a single
  write failure. That record is now retried by the same mechanism as for every
  other incident source, without re-sending the alert itself.
- `install.sh` did not apply `GOTCHA_AGENT_ENVIRONMENT` or `GOTCHA_AGENT_ROLE`:
  the documentation described them alongside the other install-time variables,
  but the script never read them or wrote them into the agent's configuration —
  exporting one before running the installer silently did nothing.

### Security
- An unauthenticated request to the login/signup forms could make the rate
  limiter remember keys that grew with the size of a submitted field, with no
  bound on the resulting memory use. Rate-limiter key length is now capped, and
  the authentication forms' request bodies are now capped in size as well.
- The rate limiters had no ceiling on the number of distinct keys they could
  accumulate: a flood of requests with distinct keys could grow a limiter's
  map without bound. Worse, all three password/SSO login entry points
  (login, registration, and SSO login forms) checked the shared per-account
  limiter before its much cheaper per-IP limiter, so a single IP submitting a
  stream of made-up email addresses to any one of them could fill that map
  in seconds — after which every real login attempt on the installation was
  denied outright once the map was full. All three now check the per-IP
  limiter first, closing that path; every limiter also enforces its own hard
  cap on distinct keys (sized to what it protects) and denies
  previously-unseen keys once at capacity, logging the event, rather than
  growing forever or silently dropping protection. Cleanup — both the
  routine kind and the one triggered by hitting capacity — is rate-limited
  to at most once per window, so it can no longer be turned into a
  full-map scan on every request once an attacker holds a limiter at its
  cap.
- Exporting events with "include PII" turned off still leaked personal data
  through fields the mask never reached: `stacktrace` (local frame variables)
  and `breadcrumbs` (URLs with query strings, HTTP crumbs) were not scrubbed at
  all, `user_id` was written out raw even though SDKs commonly put a login or
  an email address in it, and `message`/`exception_value` skipped the same
  free-text scrubber already used at ingest. All of these now go through the
  same masking as `request`/`contexts`; `user_id` is replaced by a stable
  per-export pseudonym rather than a fixed placeholder, so the file still
  supports counting unique affected users without exposing who they are.

### Documentation
- The privacy page did not mention logs anywhere — not among the personal data
  processed, and not in the description of what the subject data export/deletion
  right covers. Both are now documented.
- Added documentation pages for escalation ladders and dependency-based alert
  suppression — both features shipped without any documentation.
- `GOTCHA_RUN_EVALUATORS` was described as the gate for four background loops
  when it gates six: the SLO evaluator and the escalation scheduler were not
  named. With `web` and `ingest` deployed separately and the flag left off, SLO
  alerts and escalation never fired at all.
- The feature list in the README did not mention logs, SLOs, deploy markers,
  exports, escalations, maintenance windows, service recipes, incident groups or
  the dependency map.
- The permission table in "Teams" and the "operator" entry in the glossary lagged
  behind the role's actual permissions.
- The glossary claimed an alert rule is bound to the channels selected for it; in
  fact a rule notifies every enabled channel of the project.
- Two previously undocumented environment variables are now described:
  `GOTCHA_ESCALATION_INTERVAL` and `GOTCHA_DEPENDENCY_SETTLE_SECONDS`.
- The `GOTCHA_MAX_BUFFER_BYTES` comment in `.env.example` gave the wrong numbers:
  five writers totalling 1.25 GiB at a 256 MiB default. There are six buffers
  (the span writer holds two), and the default is derived from the container's
  cgroup limit rather than being fixed.

## [0.22.1] - 2026-08-27

### Fixed
- Exports: the `url` column of an issue-groups export pointed at a page that does
  not exist, so every link in the file returned 404. It now points at the group's
  actual page.
- Exports: on a single issue's page the export form was laid out as a row inside a
  narrow popup — the format dropdown was clipped and the submit button stretched
  into a vertical slab. The "Export PII unmasked" checkbox sat on a line above its
  own label on all four export forms.
- Exports: the button on a single issue's page is now named in Russian throughout
  ("Экспорт событий группы" instead of a half-English "Экспорт событий issue").

## [0.22.0] - 2026-08-27

### Added
- Error and event exports (`/projects/{id}/exports`): queue a background job for a
  CSV/JSON/NDJSON file of a project's error groups or raw events, filtered by time
  range/environment, with download and delete on the page. PII (email/IP) is
  masked by default; raw exports are available only to org admins/owners.
  Finished files email the author and are kept for a limited time (seven days by
  default), then cleaned up by a background janitor.

## [0.21.0] - 2026-08-26

### Added
- Incident groups (alert correlation): an availability incident of a node
  (silent host / down monitor) now anchors a group; incidents of dependent
  nodes (hosts, monitors, host-scoped metric alerts, uptime SLOs) join it
  and stay quiet while the root is informing. Members released by the root's
  recovery re-notify and escalate again — the ladder restarts from the
  moment the group closed. Root notifications carry a "Dependent nodes: N"
  line.
- Groups form around the actual root of a cascade, including failures that
  travel top-down: when a node goes silent under an already-down parent, its
  children's earlier incidents still join the top root's group — even when
  that root is a monitor.
- Incident feed (`/projects/{id}/incident-feed`): open groups with expandable
  composition, out-of-group incidents across all six sources, and a
  recently-resolved section (last 24h). The composition shows who is quiet
  because the root notifies, who is suppressed by an unreachable parent
  (two distinct states that can coincide), and who has already resolved.
  An incident that outlives its group appears among ungrouped ones right
  away, pointing back to the group it came from.

## [0.20.0] - 2026-08-21

### Added
- MariaDB recipe (mysql receiver): config, detection, charts and recommended thresholds.

## [0.19.0] - 2026-08-21

### Added
- Service monitoring recipes: ready-made OTel collector configs, live data detection, preconfigured charts and one-click recommended thresholds for PostgreSQL, nginx, Redis and Docker.

## [0.18.0] - 2026-08-20

### Added
- Node dependency suppression: declare host/monitor dependencies so a failed parent silences its children's alerts (one notification instead of a storm), with a settling grace and storm-free recovery.

## [0.17.0] - 2026-08-20

### Added
- Notification escalation: each project can define a ladder of steps per severity (critical/warning) — every step widens who gets notified after a configurable delay from when the incident opened, for host, metric, trace/profile regression, and SLO incidents alike. An operator can acknowledge an open incident from its screen to stop further escalation, and a severity badge now shows on every incident (metric alert rules can override their severity instead of inheriting the source's default). Recovery notifications go out only to the channels that actually received an escalation step. A new Escalations screen configures both ladders per project, with a dry-run preview of what each would actually send.

## [0.16.0] - 2026-08-20

### Added
- Maintenance windows now silence notifications from every source in the project — uptime, hosts, metrics, regressions, profiles, SLOs, and error alerts — instead of just monitor checks as before. Windows also gained an indefinite type: check "No end date" for a window with no end, useful for maintenance of unknown duration. Data collection and incident tracking are unaffected either way; only outbound notifications are held.

## [0.15.0] - 2026-08-20

### Added
- Host threshold overrides: each of the 4 built-in host thresholds (disk/memory/load/silence) can now be overridden per host or per group of hosts sharing an environment/role label, on top of the existing project-wide setting. The cascade resolves host → role → environment → project → default, per threshold kind, with enabled/value resolved independently — so a level can inherit one and override the other. Every level offers three states: inherit, override, or turn off. Per-host overrides live in a new block on the host's card (read-only for non-operators, showing the effective value and its source); group rules by environment or role live in a new block on the threshold settings page.

## [0.14.0] - 2026-08-19

### Added
- Host labels: hosts can now be tagged with an environment (`deployment.environment` resource attribute or the agent's `GOTCHA_AGENT_ENVIRONMENT`) and a role (`host.role` or `GOTCHA_AGENT_ROLE`), both read-only and sourced straight from telemetry. The hosts list gets environment/role facet filters plus a "new" chip (hosts younger than 24h), an optional group-by-environment/role view, and a "new" badge on both the list rows and the host's own card.

## [0.13.0] - 2026-08-19

### Added
- Seasonal baseline for the regression detector: instead of comparing against a rolling daily average, the detector can compare each metric against the same window on the same day of the week over prior weeks (default 4, configurable 2–12) — cutting false alarms on services with a pronounced daily or weekly load profile, where the rolling baseline goes silent at night and rings during the morning ramp-up. It's a per-project toggle in the "Regressions" settings card, tagged with a "Seasonal mode" badge in the regressions list, and falls back to the rolling baseline automatically for any target that doesn't have enough seasonal history yet.

## [0.12.1] - 2026-08-19

## [0.12.0] - 2026-08-19

### Added
- SLOs and error budgets: define a service level objective (a target such as 99% over a rolling 1–90 day window) on any of three indicator types — availability (share of successful requests to a transaction), latency (share of requests faster than a threshold), or uptime (share of successful monitor checks). Each SLO tracks its attainment and remaining error budget, and a two-window burn-rate alert opens an incident only when both a long and a short window are burning the budget above the threshold (default 14.4) and closes it when the short window cools — notifying through the existing alert channels. A new SLO screen lists every objective with its current attainment and budget; the detail view adds a budget-burn chart and incident history. Maintenance windows are excluded from every calculation, and each evaluation clips its window to the available telemetry retention.

## [0.11.0] - 2026-08-18

### Added
- Deployment markers: CI can report each release with a single request (`POST /api/{project}/deployments/`, authorized with the project's `sentry_key`). Reported deployments show up as version-labelled vertical markers on the performance, metrics, host, and uptime charts, in a "Deployments" list under the Performance area, and as an "after deploy vX" note next to any regression that started within 7 days after a release — so a regression can be traced back to the change that most likely introduced it.

## [0.10.0] - 2026-08-18

### Added
- Dependency map: the new "Dependencies" screen under Transactions shows a service's external dependencies (databases, caches, outbound HTTP calls) with call volume, latency (p50/p95), and error rate — derived from traces you're already collecting, no extra setup required.

## [0.9.0] - 2026-08-18

### Added
- Logs are now cross-linked with errors, traces, and hosts: an error's detail page offers "Logs around this event", a trace's waterfall offers "Logs for this trace", and a host's card offers "Host logs". The logs screen accepts a `trace_id` filter shown as a removable chip.

## [0.8.0] - 2026-08-18

## [0.7.0] - 2026-08-18

### Added
- Log ingest: OTLP/HTTP (`POST /v1/logs`, protobuf and JSON) and newline-delimited JSON (`POST /logs`) both accept structured log records, authorized with the project's public key the same way as metrics. Severity is canonicalized to a fixed six-level scale (`trace`/`debug`/`info`/`warn`/`error`/`fatal`) from either OTLP's `SeverityNumber` or a free-text level. Logs are stored in ClickHouse with their own monthly quota (`GOTCHA_DEFAULT_LOG_QUOTA`) and retention (`GOTCHA_LOG_RETENTION_DAYS`, 14 days by default). The log body goes through the same unconditional URL scrub (query-string tokens, basic-auth) as error messages, and drops from an exhausted log quota now show up in the organization's usage banner alongside events/transactions/metrics/profiles.
- A new logs screen (`/projects/{id}/logs`) makes stored logs browsable: filters for time range, severity, service, environment, and full-text search over the body; a newest-first list with expandable rows (full body, attributes, trace/span IDs); keyset ("show older") pagination; a stacked volume-by-severity histogram; built-in facets (severity, service, environment) with counts and click-to-filter; auto-discovered attribute-key facets with lazily loaded top values; and a typeahead autocomplete endpoint for attribute keys. Attribute filters (`?attr=[res:]key:value`) support exact match only, and the time window is clamped to `GOTCHA_LOG_RETENTION_DAYS`.

## [0.6.2] - 2026-08-17

### Changed
- Building the image no longer reaches the internet for Go modules: dependencies are vendored (`vendor/`) and the build runs with `-mod=vendor`, so `make up-rebuild` works in closed networks with no outbound access. Previously the build ran `go mod download` against `proxy.golang.org`, which failed behind a restricted network.

## [0.6.1] - 2026-08-17

### Fixed
- The endpoint list (`/performance`) no longer issues one ClickHouse query per row to build sparklines — all rows are fetched in a single grouped query, so the page opens without delay on projects with many endpoints.
- Clicking a "slowest trace" whose spans have already passed their retention no longer returns a bare 404: a clear "trace details are unavailable" state is shown instead (span-level detail is kept for a shorter time than the transaction summary). The retention window is read from `GOTCHA_SPAN_RETENTION_DAYS`, so the message and the link removal are correct for any configured retention, including "keep forever".
- The "Apdex threshold" field on the endpoint page described the Apdex index (0..1); its help now explains the threshold — the target response time in milliseconds. The 0..1 explanation stays on the Apdex index column.
- `GOTCHA_AGENT_DIST_RATE_PER_MIN=0` now removes the agent distribution rate limit (consistent with `0` meaning "no bound" elsewhere) instead of blocking distribution with a 429 for everyone.

### Documentation
- Added `GOTCHA_HOST_EVAL_INTERVAL` to the configuration reference (previously linked from the Hosts guide but missing). The README and upgrade guide now cover the second `gotcha-agent` binary and that host agents are updated separately from the instance.

## [0.6.0] - 2026-08-15

### Added
- A native `gotcha-agent` is now the default, recommended way to connect a host: a single dependency-free binary installed with one command run from the instance (`GOTCHA_AGENT_ENDPOINT=... GOTCHA_AGENT_KEY=... sh -c "$(curl -fsSL <base>/install.sh)"`), running as its own unprivileged systemd service. It collects the same `system.*` metrics as the shipped OpenTelemetry Collector config, plus `system.uptime`, and buffers roughly an hour of undelivered data in memory so a brief instance outage doesn't lose metrics. The binary and installer are served by the instance itself (`GOTCHA_AGENT_DIST_DIR`), so installation works in closed networks with no outbound internet access. Updating is the same command run again without the environment variables. A host's card now shows the installed agent's version and an "update available" badge when it's behind the instance.
- `system.uptime` was added to the collector config the "Hosts" onboarding hands out, so uptime shows on a host's card regardless of which collection path (agent or `otelcol-contrib`) is in use.
- The instance serves the installer and the binaries at two new public routes, `GET /install.sh` and `GET /agent/{file}` — unauthenticated (installation would be impossible otherwise) and rate-limited, with the binary route (`GET /agent/{file}`) behind its own separate per-IP limiter; if a reverse proxy with a path allowlist sits in front of the instance, add these two to it. The distribution rate limit is set by `GOTCHA_AGENT_DIST_RATE_PER_MIN` (120 requests per minute by default — about 60 hosts a minute from one IP, since an install costs two requests).

### Changed
- Host onboarding now leads with the agent as the default connection method; the OpenTelemetry Collector (`otelcol-contrib`) remains a fully supported alternative for platforms or fleets the agent doesn't cover.

## [0.5.1] - 2026-08-14

### Security
- Rebuilt on Go 1.26.6, which closes seven vulnerabilities in the Go standard library that the shipped binary was exposed to: quadratic complexity in `net/url` path resolution (GO-2026-6218), unbounded post-handshake messages in `crypto/tls` (GO-2026-6090), a missing `ReadHeaderTimeout` on unencrypted HTTP/2 in `net/http` (GO-2026-6089), unbounded recursion in the `encoding/xml` (GO-2026-6088) and `encoding/asn1` (GO-2026-5972) decoders, a parser panic on malformed SVCB/HTTPS DNS records in `golang.org/x/net/dns/dnsmessage` (GO-2026-5942), and an IDNA Punycode label check in `golang.org/x/net/idna` (GO-2026-5026). No application code changed; the toolchain floor in `go.mod` moves from 1.26.5 to 1.26.6.

## [0.5.0] - 2026-08-14

### Added
- A new "Hosts" section shows system metrics for the servers your application runs on — CPU, memory, disk, network, load average, and process count — collected via the OpenTelemetry Collector (`otelcol-contrib`) and tagged with `host.name`, kept separate from application metrics. Four built-in thresholds (disk, memory, load, and "went silent") open and notify on incidents out of the box, with sane defaults and per-project fine-tuning — no manual alert rules to write. The empty-state onboarding and threshold settings page hand you a ready-made collector config (base URL and project key already filled in) to copy onto the server. System metric names (`system.*`) are now hidden by default behind a toggle on the project's metrics list, so a connected host's metrics don't drown out application metrics there. The new `GOTCHA_HOST_EVAL_INTERVAL` variable (default 60 seconds) controls how often the background loop evaluates host thresholds. A single project holds up to 1000 hosts, and hitting that ceiling is visible in the log and in the `gotcha_host_registrations_rejected_total` counter. The evaluator's own liveness and host registration failures are now published on `/metrics` — `gotcha_host_evaluator_last_tick_timestamp_seconds`, `gotcha_host_evaluator_tick_duration_seconds`, `gotcha_host_registration_failures_total`, `gotcha_host_registrations_rejected_total`. A "Silence" incident is no longer opened for a host observed for less than the threshold (ephemeral pods), nor opened for every host at once in the first minutes after the product restarts — our own downtime no longer looks like a fleet-wide outage. When a host stops reporting past the metric-retention window it is retired rather than vanishing silently: its still-open incidents are closed with a notification, then the host is removed. When detail delivery to a channel is turned off, host alert notifications no longer carry the host name in their link — they point at the hosts list instead.

## [0.4.12] - 2026-08-12

### Added
- Project team members can now manage a project's day-to-day monitoring — monitors (create, edit, pause/resume, delete), the heartbeat ping token, maintenance windows, status-page content, issue alert rules, and metric alerts — without being promoted to organization admin. Alert channels stay owner/admin only, since a channel's recipient and secret (a bot token, an SMTP address, a webhook URL) are credentials and personal data rather than an operational setting; a team member who isn't owner/admin sees the channel list and delivery log with recipients masked instead. Status-page publication (the "Published" toggle) and project/organization settings are unchanged, also owner/admin only. A member with no team attached to a project gets the same 404 as someone with no access at all when visiting that project's operator pages. Monitor header values (e.g. an `Authorization` header on an HTTP check) are now encrypted at rest, the same way alert-channel secrets already were.
- On an issue's detail page, two buttons copy the selected event's full context to the clipboard as **Markdown** or **plain text** — the exception and message, the complete stack trace (files, lines, functions), the HTTP request (method, URL, query string, headers, body), contexts, tags, and breadcrumbs — ready to paste into an AI model for a fix suggestion, instead of copying the pieces by hand. The copy uses the clipboard API with an `execCommand` fallback for bare-HTTP LAN deployments, and works without page-level JavaScript beyond a small progressive-enhancement script.

### Changed
- A status page's public address is now an immutable, opaque key (`/status/p_...`) instead of a human-chosen slug. A slug was unique across the whole instance, so picking names at creation time doubled as a probe for whether another organization already held a given name — a cross-org leak — and let short, desirable names get squatted on; the key removes both. Existing `/status/<slug>` links keep working, answered with a 301 redirect to the new address. The slug field is gone from page creation; the page's public name is now its title.

### Fixed
- The default deployment could be OOM-killed under sustained load with a slow ClickHouse. The event and span writer buffers were capped by row count, but a client controls row size, so the buffers could outgrow the container's memory limit before that cap engaged. The buffer byte-ceiling is now derived automatically from the detected memory limit (still overridable with `GOTCHA_MAX_BUFFER_BYTES`), and the ingest queue's byte accounting now includes the span data and tags it previously ignored.
- Dropped events no longer disappear from usage. Events lost to a full queue, a storage error, or a handler panic — and events dropped when a writer buffer overflows under overload — are now attributed to the organization in `org_usage.dropped_*`, so usage reflects real losses instead of hiding them.
- A delivery-channel secret that can't be decrypted (for example after `GOTCHA_SECRET_KEY` is rolled back to a development key) is no longer treated as a live secret: the channel is flagged as needing attention instead of silently shipping ciphertext and failing delivery.
- The alert for an issue's first occurrence is no longer lost when throttling runs before the alert is enqueued, and the notification worker now recovers from a sender that panics instead of taking the delivery loop down with it.
- Monitor fixes: an HTTP `HEAD` check combined with a response-body assertion no longer always fails validation; maintenance-window and uptime-consensus edge cases were corrected; and the surface that could expose a monitor's stored header values to a non-admin operator was narrowed and is now covered by a regression guard.
- An OAuth invitation is no longer consumed when identity linking fails after the invite was accepted: the just-created account is rolled back and the invitation stays pending, so the invitee can sign in again without being re-invited.
- `?period=all` no longer renders an empty page on the issue, monitor, and other time-ranged views.

## [0.4.11] - 2026-08-10

## [0.4.10] - 2026-08-05

### Testing
- The shared PostgreSQL and ClickHouse containers are no longer swept out from under a running suite. The container reaper owns containers per session — one `go test` invocation — and removes them ten seconds after that session's last process disconnects. Ours are shared and reused by name, both across the packages of one run and across runs, so neither boundary fits that ownership: between two package binaries nobody is connected to the reaper, and with `-p 1` (mandatory, since starting containers in parallel brings the machine down) that gap is the compilation of the next binary — tens of seconds under `-race`. It swept two CI runs on 2026-08-05 mid-flight, in packages nothing had touched. The reaper is now off for these containers and `make test-env-down` removes them, so their lifetime is stated rather than raced for.

## [0.4.9] - 2026-08-05

### Interface
- Checkboxes and radio buttons are drawn by the product instead of the browser. The native control's own border measures about 1.06:1 against the surface — a third of the contrast WCAG 1.4.11 asks of a control boundary — so an empty checkbox was barely visible, and the previous remedy drew a border around the native square. That never sat right: the browser paints its fill inset from the element's edge, so the extra line ran around the control with a gap instead of along it, and read as a stray frame. With the control drawn from scratch the border *is* its edge, the checkmark is a shape rather than a font glyph, and both states look deliberate in either theme. The 18×18 target size now also survives inside labelled checkboxes, where an override had been quietly restoring the browser's 13×13.
- A checkbox with an explanatory hint gets a layout of its own. Reusing the text-field wrapper put the box above its own caption in dimmed small type, because that wrapper stacks a label over its input — correct for a text field, wrong for a checkbox.

## [0.4.8] - 2026-08-05

### Documentation
- The `GOTCHA_NET_MTU` section called the MTU mismatch almost always harmless because the kernel reports the narrow link and the connection adapts. That holds for the outbound direction only. Inbound packets are sized by the remote end from the MSS the container advertised, and it shrinks them only on an ICMP "fragmentation needed" from a router on the path — which is neither your side nor your control. The section now derives the symptoms from that: some destinations work while others hang, the failure can vanish for ten minutes and return with the route cache, and the host always works because its own interface is already narrow. That last point is the trap, so the check is now the one that discriminates — a TLS request from inside the container, not from the host.

## [0.4.7] - 2026-08-05

### Fixed
- Checked checkboxes and radio buttons no longer carry a grey frame around them. The resting border exists so that an *empty* control stays visible — the browser's own is far below the contrast floor (WCAG 1.4.11) — but it was drawn in every state, and on a checked control the browser paints its fill inset from the element's edge, so the line landed around the fill with a gap rather than along it. It now applies only while the control is unchecked; the focus ring is unaffected.

## [0.4.6] - 2026-08-05

### Added
- A channel can be marked as **"This recipient is inside my perimeter"**, and details then reach it regardless of what its address looks like. The detail policy decides per recipient, and a Telegram `chat_id` carries nothing to decide on, so such a channel stayed external forever — on a self-hosted instance, where the operator and the recipient are the same person, the only lever left was `GOTCHA_EXTERNAL_CHANNEL_DETAILS`, which opens details to every channel of every project at once. The box is ticked by hand, one channel at a time, and is off by default, like the rest of the policy; channels carrying it show a "With details" badge in the table, so who receives personal data is visible without opening each channel in turn.

### Documentation
- The privacy page claimed the Telegram API endpoint could only be changed by editing the code. It has been a setting since 0.4.5 — the sentence outlived the release that made it false.
- `GOTCHA_NET_MTU` presented the MTU mismatch between the host uplink and the container network as the diagnosis. The mismatch is ordinary and almost always harmless: when the narrow link is the host's own interface, the kernel reports it and the connection adapts. The page now says so, and names the pair of signs that means the MTU really is at fault — large exchanges timing out while small ones pass, *and* an uplink below 1500 — plus what to do when the change doesn't help.

## [0.4.5] - 2026-08-05

### Added
- `GOTCHA_TELEGRAM_API_BASE` points Telegram delivery at a Bot API address of your choosing — your own `telegram-bot-api` server, or a reverse proxy on a network from which Telegram is reachable. Instances behind traffic filtering or a closed egress had one remedy, pinning `api.telegram.org` to whichever IP still answered through `extra_hosts`, which holds only until the addresses or the filtering rules change. An invalid value stops the instance at startup instead of turning into a timeout on every delivery.

- `GOTCHA_NET_MTU` sets the MTU of the container network, which Docker fixes at 1500 without looking at the host's uplink. A VPS behind a tunnel commonly has 1450, and then the container advertises an MSS the path cannot carry: small exchanges work, the first large one — a TLS handshake — is dropped silently, and the failure reads as a timeout on a connection that plainly exists. It affects everything outbound at once, so a monitored site can be reported unreachable because of the MTU of the container watching it.

### Documentation
- The app container's compose-only variables are documented: `GOTCHA_NET_MTU` with the symptom, the check (`ip -o link show`) and the fix, and `GOTCHA_MEM_LIMIT`, which existed since the ceilings were introduced but appeared in no reference file.
- The Telegram troubleshooting section explained the symptom through a single cause — IPv6 resolution without global IPv6 — and presented the IP pin as the cure. It now separates name resolution from reachability, gives the check for each, and orders the three remedies by how long they last, saying plainly that the pin is a stopgap. Outbound proxies (`HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`) are documented for the first time, including where they deliberately do not apply: webhook channels and uptime checks dial their targets directly so the SSRF filter keeps deciding on the address actually connected to.

## [0.4.4] - 2026-08-04

### Security
- The redirect guard at the `Location` header rejects control characters, closing a third form of the open-redirect bypass it already blocked in two others. Browsers strip tabs and newlines from a URL before parsing it (WHATWG URL), so `/<TAB>/evil.example` reaches the parser as `//evil.example` — a protocol-relative address pointing at another host — while passing the checks for `//` and `/\`. Go leaves such a byte in the header: only non-ASCII is escaped, and the `\r\n`-to-space replacement happens in the header serializer. No reachable exploit existed, because both callers pass the value through `url.Parse` first (it rejects control bytes), but that made the guard depend on its caller — exactly what it exists not to do. Reported by CodeQL as `go/unvalidated-url-redirection`.

## [0.4.3] - 2026-08-04

### Fixed
- Anonymized notification subjects are human-readable. A recipient outside the trusted perimeter used to get the raw event enum with a lowercase prefix — `[gotcha] new_issue` — instead of the `[Gotcha]` prefix and human captions every other subject carries. Redacted subjects and bodies now say what happened («[Gotcha] Новая проблема», "[Gotcha] Monitor is down") in the instance language, still without any details; a completeness test holds the label dictionary to every alert kind of every notifier.
- The sign-up page copy matches the registration mode. In `invite` mode the form warned about nothing and the dead end surfaced only after submitting; it now says up front that sign-up is invite-only and to use the link from the invitation email — the warning disappears when the visitor arrives by such a link. In `closed` mode the info page borrowed the invite-mode text and advised getting an invitation, which does not help there — it now honestly says new accounts are not created. A rejected submission shows one message instead of two nearly identical paragraphs.
- The invitation email names the organization and the inviter. The subject read "Invitation to a Gotcha organization" — on the Russian locale it read as an organization literally named «Gotcha» — and the body did not say who invites or where; on a multi-organization instance the recipient could not tell what they were joining.
- Large metric values are written compactly — `910M`, `1.25G` — instead of scientific notation: thresholds and incident values on the metric alerts page showed `avg > 8e+08`, unreadable for bytes. Chart axes shared the defect one step later (a value from a billion rendered as `1e+03M`) and use the same compact format now.
- The transactions table indicates its default sort. The list arrives sorted by throughput, but no column carried `aria-sort` or the arrow until an explicit click on a header.

## [0.4.2] - 2026-08-04

### Added
- `--migrate-force=N` and `--migrate-force-ch=N` clear the dirty flag an interrupted migration leaves behind. The error used to advise `migrate force N` — a binary the image does not ship. Only two targets are accepted, the current version and the previous one, because anything else is a typo that would silently shift the starting point of every future migration; the flag does not finish the migration, and says so. The startup errors now name the real command, and the upgrade docs walk through the whole situation.
- The compose stack is tunable through four substitution variables: `GOTCHA_PG_PASSWORD` and `GOTCHA_CH_PASSWORD` reach both the database container and the app DSN from one place (changing them on an existing install requires `ALTER USER` first — the procedure is in the configuration docs), `GOTCHA_PG_MEM_LIMIT` and `GOTCHA_CH_MEM_LIMIT` raise the database memory ceilings without editing the file.
- Events dropped because PostgreSQL was unavailable are now counted (`reason="storage_error"`). They were only logged, so the documented rule "no drops means events never arrived" sent operators to check their SDK during a database outage.
- The heap ceiling is derived from the container's memory limit at startup, in any deployment that sets one — compose, Kubernetes, systemd. Go does not read cgroup limits, so buffers used to grow past the limit until the kernel's OOM killer discarded all of them. An explicit `GOMEMLIMIT` still wins. Reported as `gotcha_memory_limit_bytes`.
- `docker-compose.yml` sets `mem_limit` (1 GB by default, `GOTCHA_MEM_LIMIT` to change) and a container healthcheck; the image gained `HEALTHCHECK`. A hung process used to sit `Up` forever, and there was no readiness signal during first-boot migrations.
- Delivery metrics: `gotcha_notify_sent_total`, `_failed_total`, `_retried_total`, `gotcha_notify_pending_jobs`, `_failed_jobs`, and `gotcha_notify_oldest_pending_age_seconds` — the last one is what tells an idle queue from a stuck one.
- Retention now covers summary records in PostgreSQL: issues and performance issues whose last event is older than `GOTCHA_RETENTION_DAYS`, and closed incidents and regressions. Open incidents are never deleted. Deletions are counted by `gotcha_entities_purged_total`.
- Schema versions carry a backward-compatibility marker, recorded in the database as migrations are applied. An older binary can now start against a newer schema when every migration it is missing is additive — rolling a release back no longer requires restoring from a backup. See Upgrading.
- `GOTCHA_INGEST_RATE_LIMIT`: the per-DSN ingest rate limit — previously a hard-coded 500 requests per second per project — is configurable; burst stays at twice the limit, and `0` disables it.

### Changed
- The database containers have memory ceilings (512m for PostgreSQL, 2g for ClickHouse by default). Without a cgroup limit ClickHouse assumes 90% of the **host's** memory is its own — on the recommended 4 GB that is 3.6 GB on top of the app's gigabyte, and the night-time OOM killer picks the fattest process. Both database healthchecks get a `start_period`, so the first boot on a minimal VPS no longer fails as "dependency failed to start" while the database was ten seconds from ready. The app container is hardened: read-only filesystem, all capabilities dropped, `no-new-privileges`, a tmpfs `/tmp`, and a pids limit — the full operator path (registration, project creation, event ingest) is verified to work under all of it.
- A build without git metadata says so instead of impersonating a release. `/version` and `gotcha_build_info` carry a `stamped` field, the About page shows a note, and the version string reads "0.4.1 (no build metadata)" — an image built with bare `docker compose build` produced a string indistinguishable from a stamped release, so "deployed exactly what you think" was unverifiable. The documented build path is `make up-rebuild`, which computes and stamps the git version.
- Each summary record in PostgreSQL is now purged by the retention of the data it describes, not by `GOTCHA_RETENTION_DAYS` alone. Metric incidents follow `GOTCHA_METRIC_RETENTION_DAYS`, profile regressions follow `GOTCHA_PROFILE_RETENTION_DAYS`, and resolved uptime incidents get their own `GOTCHA_INCIDENT_RETENTION_DAYS` (90 days by default) because they have no telemetry of their own while the public status page promises ninety days of history. **Resolved profile regressions are affected visibly:** with the defaults they are now removed after 7 days instead of 90, and on an instance with `GOTCHA_PROFILE_RETENTION_DAYS=1` after a day. A regression card whose samples are already gone shows nothing, which is why they should not outlive them; raise `GOTCHA_PROFILE_RETENTION_DAYS` if you need them kept longer.
- `/healthz` now answers 200 whenever the process serves HTTP, and no longer returns 503 when a database is unreachable. Readiness moved to the new `/readyz`. Pointing a liveness probe at the old behaviour restarted a healthy container on every storage outage, and each restart threw away the buffers that were waiting for storage to come back. Update anything that watches `/healthz` for readiness.
- Notifications are delivered concurrently (`GOTCHA_NOTIFY_CONCURRENCY`, default 4) and the worker keeps claiming batches while the queue is not empty. One dead webhook holding a 30-second timeout no longer delays every other notification.
- `gotcha_pipeline_dropped_tasks_total` gained a `reason` label: `queue_full`, `queue_bytes`, `storage_error`, `panic`, `closed`.
- Retention settings accept `0` as "keep forever": the ClickHouse TTL is removed instead of expiring data, and the configuration docs said so before the code allowed it. `GOTCHA_OUTBOX_RETENTION_DAYS` keeps its `>= 1` floor deliberately — the outbox is a working queue, not an archive.

### Fixed
- Outgoing notifications speak one language, chosen by the operator: `GOTCHA_LOCALE` (`ru` or `en`, default `ru`). The six senders used to write in a random mix — performance regressions in Russian, uptime and metric alerts in English, performance alerts glued an English frame around a Russian title. On a default (`ru`) instance uptime, metric, profile and issue alerts and the suppressed-alerts digest now arrive in Russian; the raw event enum in issue alert subjects (`new_issue: …`) became a human label, and the `[gotcha]` prefix is `[Gotcha]` everywhere.
- Performance finding titles are built at render time in the viewer's language. Detectors used to bake a Russian title into the stored row, which then leaked into the English UI and into notifications as-is. The row now keeps the kind and the parameter (migration `0058` extracts parameters from accumulated rows), so the list, the detail page, `<title>` and notifications agree with the viewer's — or the instance's — locale. Old rows that match no known prefix still show their stored text rather than a caption with no parameter. The i18n leak-debt list is burned down to an empty list with a ceiling of zero.
- The Yandex login button is captioned by locale — "Yandex" on the English UI, «Яндекс» on the Russian one. Provider captions come from the catalog; a generic OIDC provider with a custom display name is shown exactly as configured.
- A user with no accessible projects is not stuck: an owner/admin sees a create-project door instead of "ask your administrator" (they are the administrator), and every logged-in chromeless page — that dead end included — has a log-out button. An empty project list leads to the create-project modal on the same page instead of the first-run wizard.
- The issues list distinguishes "no events yet" from "nothing matches the filters": the second case now says so and offers a one-click filter reset instead of suggesting to connect the SDK to a project that is already reporting.
- Opening `/register` on a closed instance shows one informational paragraph; the same text is no longer duplicated as a red error banner. The banner remains for actual rejected submissions.
- A member whose role is insufficient gets an honest 403 on management pages — they already know the organization exists, and "not found" on a familiar page reads as breakage. 404 stays for non-members, so existence of other people's organizations is still not revealed.
- The performance section is called "Transactions" everywhere — rail, sidebar, heading, page title, breadcrumbs. "Endpoints" remains only as the table heading, where it names the data, not the section.
- The selected time window sticks: picking a preset stores it in a cookie and it becomes the default on the other pages, so navigating no longer silently resets the window. An explicit query always wins; custom date ranges and "all time" are deliberately not persisted.
- Switching projects keeps the current section — from Transactions of one project to Transactions of the other, falling back to issues when the section has no page for the target project. The probes breadcrumb is captioned "Organization settings" — where it actually leads — instead of "Members".
- Teams can be deleted. The action is confirmed on a separate page naming the team; memberships and project links go with it. Detaching a project from a team — which instantly revokes the whole team's access — now also asks for confirmation naming both sides.
- A notification channel can be tested right from the alerts page. The test message is sent synchronously, bypassing the queue: success is a flash, a delivery failure is shown with its reason — no more waiting for a real incident to find out the webhook URL had a typo.
- The invite form survives a validation error: the typed email and chosen role stay in place and the error is shown next to the form, not as a paragraph under the page heading.
- The getting-started checklist can be hidden permanently (a profile flag, migration `0059` — it does not resurrect on another device), and "invite your team or add a monitor" became two separate steps, each linking where it says.
- Platform normalization moved into the domain (`CreateProject`), so both creation paths — onboarding and the projects-page modal — get it; the modal used to store the raw form value.
- Error and flash messages follow one canon: capitalized, no trailing period. 224 catalog values were normalized and a guard keeps new ones in line.
- Without JavaScript a flash message no longer self-destructs: the auto-dismiss animation is gated on JS, matching the close button that never rendered without it — previously the message vanished forever with no way to have read it.
- The probe run command explains the `<gotcha-image>` placeholder and links the probes documentation; the checkbox column of the issues table has a screen-reader caption.
- The availability bar states are distinguishable without colour vision. The three fills used the text status tokens, which are all pinned to the same 4.5:1 text contrast and therefore nearly identical in lightness — under any colour-vision deficiency the bar read as one solid colour. The fills now have their own tokens spread apart in lightness (every pair at 3:1 or better, held by a test), and the "intermittent" bucket additionally carries a diagonal hatch pattern — a second channel that survives even monochrome rendering.
- An availability bucket where most checks passed is no longer captioned "down". The bar class and the tooltip caption were computed by two separate switches that had drifted apart: a bucket with 9 of 10 checks passing was painted yellow but announced as down. Both now derive from the same class-to-caption table, with a new honest caption for the intermittent state, and a completeness check fails the build if a class ever lacks a caption.
- The latency phase colours (DNS → TCP → TLS → TTFB) are readable in the light theme. The four fills were dark-theme literals baked into two places — the chart and its legend could not drift apart visually, but the lightest phase gave 1.81:1 against a white card. The phases are theme tokens now, each at 3:1 or better against the card, with the lightness still growing monotonically along the request timeline in both themes.
- The selected segment of the "One-off / Weekly" control meets text contrast. Its caption was painted with the accent token — designed as a button background under white text, not as text itself — giving 2.98:1 on the tinted background. The active segment now uses the link colour plus a translucent link-coloured border, the exact treatment of the language and theme switchers.
- Interactive control borders meet the 3:1 non-text contrast requirement everywhere. Seventeen selectors — the date-range picker family, issue filters, monitor and bulk-action buttons, the segmented control, the global input rule and others — still used the decorative border token (1.4:1 in the dark theme); all of them now use the control border token, and the guard's debt list for this rule is empty with a ceiling of zero.
- Every scrolling container is reachable and announced. Tables, chart panes, waterfalls and flame graphs scrolled only with a mouse: a keyboard user could not reach the container, and a screen reader announced nothing about it. All forty of them now render through one component that adds `tabindex`, `role="region"` and a label naming what scrolls, a visible focus ring, and a guard that rejects any literal scroll-class markup. Documentation tables get the same wrapper from the markdown renderer — and their `display: block` workaround is gone, so they are tables again for assistive technology instead of a stream of text.
- Empty states continue the page's heading structure instead of always rendering an `<h3>` — a list page whose only content was an empty state jumped from `<h1>` straight to `<h3>`.
- The "Window type" radio pair is a `fieldset` with a `legend`, so a screen reader announces what the two radios choose between; visually nothing changed.
- A modal reopened by the server after a validation error moves focus to its heading — the keyboard user used to stay at the top of the page with the error invisible; closing a modal returns focus to the element that opened it, and the error message is linked to the field it concerns via `aria-describedby`/`aria-invalid` instead of a `role="alert"` that never fired.
- The language switcher carries `aria-pressed` like the theme switcher; the three status-filter tab bars on the performance pages have accessible names instead of being anonymous "navigation" landmarks (a guard keeps them named).
- The pending-invitations block styled its hint text and revoke button with classes that do not exist in the stylesheet (`muted`, `btn-sm`) — the text now uses the standard hint style and the button the outlined danger style, and both classes left the undefined-class debt list.
- The date-range picker popup is no longer clipped on short pages. The content column carried `overflow-x: auto` — which by CSS rules also turns vertical overflow into a scroller — so on a page shorter than the calendar the popup was cut off inside a barely-scrollable column. Now that every wide element sits in its own scroll region the column-level scroller is gone: the page grows and the whole calendar is visible. Removing it also surfaced a set of narrow-screen (375px) overflows that the column scroller had been masking; all are fixed at the source — the issue events and endpoint web-vitals tables joined the scroll regions, tab rows scroll within themselves, the quota banner and issue meta wrap, long inline code breaks, and the screen-reader-only span no longer widens the document.
- Issue and performance-issue pages showed the literal text `@relativeTime(...)` instead of the "first seen / last seen" timestamps: the component call sat on the same line as the label text, where the template engine treats it as plain text.
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
- A public status page never shows more history than is actually retained: the window is capped by `GOTCHA_RETENTION_DAYS`. Shortening retention used to leave the page drawing 90 cells, most of them empty, which reads as a monitoring failure rather than a retention setting.
- Changing retention now logs a warning naming the old and new values. TTL is a property of the installation but is configured per replica, so replicas that disagree flip it back and forth — and every flip rewrites every part of the table.
- Shutting the process down no longer leaves up to five notifications waiting for the claim lease: the result of an attempt that already happened is written regardless of cancellation.
- The next retry time for a notification is computed by the database clock rather than the process clock.
- Claim windows for the notification outbox, alert throttling, and the alert budget are now computed by the database clock. A process whose clock ran fast could take another worker's lease and send an alert twice.
- Weekly maintenance windows keep the duration you configured across daylight-saving transitions. A 02:00–04:00 window collapsed to one hour on the spring transition and started an hour late on the autumn one, missing the first check.
- Open registration mode now provisions an account on the first OAuth sign-in without an invitation, as the docs promised — OAuth used to demand a pending invitation even under `open`. A pending invitation for the address, if any, is accepted along the way; `invite` and `closed` are unchanged.
- An event carrying a NUL byte (0x00) in any string field is stored instead of being lost. JSON allows NUL, PostgreSQL text columns do not: ingest accepted such an event with `200 OK` and then dropped it on the issue write — the sender believed it was delivered. NUL bytes are now stripped from every untrusted string on all ingest surfaces (events, transactions, OTLP, profiles).

### Security
- OTLP span attributes are capped before the maps are built, not after. A 10 MiB body (about 30 KB gzipped) carries roughly 1.2 million attributes, and parsing them cost about 100 MB on top of the body — measured at 184 MB for a million attributes. The cap is 256 attributes per span; `maxDataKeys` still applies among those.
- `hostOfURL` requires an absolute http(s) URL, so a webhook target like `ftp://…` or `//evil.example/x` no longer resolves to a trusted host when deciding whether event details may be sent.
- The post-login redirect is validated where the `Location` header is set, not only where the form field is parsed: `next` was sanitised on the way in and handed to `http.Redirect` unchecked twenty lines later. Both checks now share `isLocalPath`.
- Removing a member from an organization now revokes access to their teams' projects too. Removal used to clear only the organization membership row; the matching row in `team_members` stayed behind and kept granting access to issues, traces, profiles, and ingest keys. The invariant is now enforced by the schema (migration `0029`): a team membership cannot exist without the matching organization membership. The migration deletes any dangling team memberships that had already built up.
- Password registration by invitation now requires the token from the invite link, not just a matching email. In `invite` mode, an email match against a live invitation alone used to be enough — anyone who knew the invited address could register and receive the invitation's role without proving they controlled that address.
- OTLP/JSON request bodies nested deeper than 100 levels are now rejected with `400`. The body walk was unbounded recursion, so a gzipped body of a few kilobytes could crash the whole process with a fatal stack overflow.
- `POST /onboarding` now applies the same rule as `GET`: onboarding is for someone who has no project yet. Only the form was gated, so an invited member could create organizations, projects, and ingest keys in a loop.
- Ingest and service routes (`/healthz`, `/readyz`, `/version`, `/metrics`) answer with `X-Content-Type-Options: nosniff` and `X-Frame-Options: DENY`. They are registered on the root mux and by Go 1.22 routing precedence they shadow `/`, so they bypassed the security-header middleware entirely — while ingest also sends `Access-Control-Allow-Origin: *`.
- A monitor can only be assigned to a region the organization actually has. The form offered only valid regions, but the POST accepted any string, and a monitor in a region nobody leases is never checked — which looks like "no failures" rather than "no monitoring".
- The "back" breadcrumb rejects protocol-relative referers in the `/\` form, matching the three sibling functions that already did.
- Delivery channel secrets no longer travel in the notification outbox payload: the worker fetches the secret by channel id at send time, and migration `0025` strips the values already sitting in queued jobs. Upgrading from older versions calls for a one-off secret rotation — see /docs/upgrade (recorded retroactively: shipped in an earlier release).
- Alert notification detail exposure switched to a per-recipient trust policy: event details reach trusted recipients only, everyone else gets an anonymized message with a link; `GOTCHA_EXTERNAL_CHANNEL_DETAILS=true` lifts the restriction (recorded retroactively: shipped in an earlier release — see /docs/alerts).

### Performance
- The spike detector now asks ClickHouse once per rule instead of once per active issue. A project with 10,000 active issues cost 10,000 round trips every minute — and the number of issues is set by whoever sends events, through unique fingerprints.
- Regression evaluators batch their queries: profiles go from 1 + 2K queries per service to two, and traces from over two hundred per project to six. The profile evaluator also stopped walking every project in the installation — it now works from the services that actually have data.
- The cardinality guard caps the number of distinct field NAMES per project (200) and the total number of remembered values (about a million, `gotcha_cardinality_tracked_values`). Field names come from the sender just like values do, so "one field per event" bypassed the guard entirely.

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
- Documentation reconciled with actual behavior across the board: retention semantics (including the shared `check_results` TTL and the status-page window), ingest rate limiting, registration modes at OAuth sign-in, `/readyz` versus `/healthz` in README and CONTRIBUTING, supported versions in SECURITY.md, time-range stickiness on the issues list, Russian labels for alert throttling, heartbeat grace and region consensus, the invite lifecycle, the container healthcheck URL flag, and the self-monitoring metrics list.

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
