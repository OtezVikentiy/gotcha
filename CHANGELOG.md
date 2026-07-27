[English](CHANGELOG.md) · [Русский](CHANGELOG.ru.md)

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project intends to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once tagged releases begin.

## [Unreleased]

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
