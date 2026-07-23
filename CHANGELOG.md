[English](CHANGELOG.md) · [Русский](CHANGELOG.ru.md)

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project intends to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once tagged releases begin.

## [Unreleased]

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
