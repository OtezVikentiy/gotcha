# Glossary

A short dictionary of Gotcha's terms — from organizations and projects to profiles and quotas. Where a term has its own detail page, it's linked.

**Organization** — the top level: groups projects, members (with roles), and shared settings (ingest limits, SSO, alert-channel templates). A user can belong to several organizations. Created together with the first project during `/onboarding`.

**Project** — a single application or service inside an organization. Each project has its own DSN keys, its own issues, transactions, metrics, profiles, and settings (sampling, regression thresholds, retention). Events from two different projects are never mixed.

**Team** — a group of organization members sharing access to a subset of projects. Used so you don't have to grant organization-wide access to everyone who only needs a couple of projects.

**DSN** — a project's connection string, shaped like `https://<public_key>@<your-gotcha-host>/<project_id>`, which you pass to the SDK at init time. It carries a public key (Gotcha resolves the project and organization from it) and the ingest address; unlike the older Sentry DSN format, there's no secret component. You can find and reissue a DSN under "Project settings" or on the "Setup" page — see [SDK & Integrations](/docs/sdk).

**Event** — a single occurrence: an error with a stack trace, level, tags, context, user data, and SDK info, sent to Gotcha over the Sentry ingestion protocol. Multiple events sharing a fingerprint collapse into one issue.

**Issue** — a group of identical events collapsed by fingerprint: one card instead of a stream of duplicates. An issue has a status (unresolved/resolved/ignored), an assignee, and an event count. More: [Issues](/docs/issues).

**Grouping / fingerprint** — the key events are grouped by into an issue, computed from the error type, message, and code location (call stack). A new event with an already-seen fingerprint bumps the existing issue's counter; a new fingerprint opens a new issue (or reopens a resolved one — that's what a regression is).

**Transaction** — a unit of performance: one handled request or operation as a whole, with an overall duration and status. Sent by the SDK when tracing is enabled (`traces_sample_rate`/`tracesSampleRate` > 0). More: [Performance](/docs/performance).

**Trace** — an end-to-end chain of related transactions and spans for a single request, including across service boundaries (when `trace_id` is propagated between them). From an issue's detail page you can jump to the related trace if the error happened inside a traced request.

**Span** — a nested segment inside a transaction: a database query, an HTTP call to another service, a template render, and so on. A transaction is essentially a root span plus a tree of children.

**Web Vitals** — page-speed metrics measured in a real user's browser: LCP, INP, CLS, FCP, TTFB. Shown with a p75 percentile and a good/needs-improvement/poor rating. Collected automatically by the browser SDK when tracing is enabled. More: the Web Vitals tab under [Performance](/docs/performance).

**Metric** — a numeric time series (counter/gauge/histogram) with arbitrary labels (environment, release, region, etc.), sent over the OTLP protocol. Viewable as p50/p95/avg/sum aggregations over a period. More: [Metrics](/docs/metrics).

**Profile / flame graph** — a snapshot of where CPU time or execution time goes inside your application during a real request, captured by the SDK (Sentry profiling) or uploaded directly in pprof format. A flame graph visualizes a profile as a call stack: a block's width is its share of time, and nesting is call depth. More: [Profiling](/docs/profiling).

**Uptime monitor** — a periodic availability check (HTTP/TCP/DNS/heartbeat) run on a schedule. Several consecutive failed checks open an incident. More: [Uptime](/docs/uptime).

**Incident** — a period during which a monitor or a regression signal is considered "down": it has a start, an optional end, and a history of checks/events within it. An incident can trigger an alert and update a public status page.

**Alert channel** — a delivery method for notifications: email, webhook, or a Telegram bot. Configured once per project and reused across multiple rules.

**Alert rule** — a condition to react to: a new issue, a regression (an issue reopening), a spike in errors over a time window, or a metric crossing a threshold. Attached to one or more delivery channels; a given rule's firings are throttled so the team isn't flooded with duplicates. More: [Alerts](/docs/alerts).

**Regression** — a statistically significant degradation of a metric (endpoint latency, Web Vitals, time spent in a profile) relative to a baseline from a prior period, detected automatically. Performance regressions show up as issues under [Performance](/docs/performance).

**Retention** — how long a given kind of data (events/transactions, trace spans, metrics, profiles) is kept in ClickHouse before it's deleted. Has an instance-wide default and can be narrowed (but not extended) per project in "Project settings".

**Quota** — a monthly cap on ingesting a given kind of data (events, transactions, metrics, profiles) per organization. Once exhausted, new items of that kind are rejected (ingest responds `429`) until the next month starts; already-ingested data is not deleted. Configured under "Organization settings → Usage & rate limits"; `0` means unlimited.

**Environment** — an arbitrary string (`production`, `staging`, `dev`, …) the SDK attaches to every event, transaction, and metric. Used for filtering across nearly every section, so production errors don't drown among local-dev noise.

**Release** — a version identifier for your application (e.g. a git hash or a build number) that the SDK attaches to events and transactions. Lets you see which release an issue first appeared in, or in which release a performance regression started.
