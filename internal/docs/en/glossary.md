# Glossary

**Issue** — a group of identical events (errors) collapsed by fingerprint. An issue has a status (unresolved/resolved/ignored) and an assignee.

**Event** — a single occurrence: an error with a stack trace, tags, context, user data, and SDK info.

**Fingerprint** — the grouping key: identical errors sharing a fingerprint land in the same issue.

**DSN** — a project's connection string, configured in the SDK. Contains a public key and the ingest endpoint address.

**Transaction** — a unit of performance: one handled request/operation with a duration. Made up of spans.

**Span** — a nested segment within a transaction (a database query, an HTTP call).

**Trace** — an end-to-end chain of related spans/transactions for a single request.

**Web Vitals** — browser page-speed metrics: LCP, INP, CLS, FCP, TTFB (p75).

**Apdex** — a satisfaction index based on response time relative to a threshold T.

**Metric** — a numeric time series (counter/gauge/histogram) with labels; aggregated as p50/p95/avg/sum.

**Uptime monitor** — a periodic availability check (HTTP/TCP/DNS/heartbeat). A failure opens an **incident**.

**Alert** — a rule plus a delivery channel: a notification about a new issue, a regression, or a spike.

**Regression** — a statistically significant degradation (latency/Web Vitals) relative to a baseline.

**Profile / flame graph** — a snapshot of where CPU/execution time goes across the call stack.
