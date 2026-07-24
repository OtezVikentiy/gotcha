[English](CHANGELOG.md) · [Русский](CHANGELOG.ru.md)

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project intends to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once tagged releases begin.

## [Unreleased]

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
