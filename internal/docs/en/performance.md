# Performance

The "Performance" section isn't about individual errors — it's about **how fast** your application is: transactions (a handled request or a background operation), made up of spans (a DB query, an HTTP call to another service, template rendering, and so on), and traces — end-to-end chains of the transactions/spans belonging to a single request.

## How to send the data

Transactions and spans are sent by your application's SDK — the same official Sentry SDK you use for errors (see [SDK and integrations](/docs/sdk)), with tracing turned on. The key setting at init time is `tracesSampleRate` (the share of requests the SDK traces and sends, from 0 to 1):

```js
Sentry.init({
  dsn: "<YOUR_DSN>",
  tracesSampleRate: 1.0, // 100% to start; usually lowered in production
});
```

Every language's SDK (Go/PHP/Python, etc.) has an equivalent option — check your Sentry SDK's docs for the exact option name and how to enable framework auto-instrumentation. As long as `tracesSampleRate` is zero or unset, the "Performance" section stays empty — data only shows up once the SDK actually starts sending traces.

## Endpoint list

The section opens from the **"Performance"** entry in the left rail — URL `/projects/<id>/performance` (the subsection is labeled **"Transactions"**). Filters at the top are environment and period (1 hour / 24 hours / 7 days / 30 days). The endpoint table:

| Column | Meaning |
|---|---|
| Endpoint | transaction name (click through to the endpoint page) |
| Throughput | transactions per minute |
| p50 / p75 / p95 / p99 | duration percentiles |
| Failure | share of transactions with a status other than `ok` |
| Apdex | satisfaction index, 0..1 |
| p95 (period) | a sparkline of p95 over time |

Column headers are links; clicking one sorts the list by that column. The endpoint list is capped in size — if there are more real endpoints than that, a note above the table reads "showing the first N of M."

**Apdex.** Below the filters the threshold `T` in use is shown (e.g. "Apdex T = 300ms", configurable in project settings). A request that finishes within `T` counts as "satisfied," between `T` and `4T` as "tolerating," slower than that as "frustrated"; Apdex = (satisfied + tolerating/2) / total. A sharp rise in p95/p99 with steady traffic is a typical sign of a degradation rather than just more load — traffic didn't go up, so it isn't about volume.

## Endpoint detail

Clicking an endpoint's name opens `/projects/<id>/performance/<transaction>`: period/environment/Apdex threshold in the header, a Web Vitals panel (if this is a page-load transaction — more on that below), a latency chart (p50/p95 over time), a throughput chart, a duration histogram, a table of the slowest traces for the period (a link to each one's waterfall), and a list of related performance issues.

**Performance issues** (N+1 queries, slow DB queries, HTTP floods) are a separate, automatically detected category of findings inside transactions — not the same thing as the regressions covered later in this doc. The full list lives in the **"Performance Issues"** section (`/projects/<id>/perf-issues`), reachable from the same "Performance" subsection menu.

## Trace waterfall

The "Trace" link from the slowest-traces table, or from an event's detail in [Issues](/docs/issues), opens `/traces/<trace_id>`: trace id, total duration, timestamp, and below that a waterfall of the span tree (each span is a bar offset and sized by time relative to the start of the trace; spans that ended in an error are marked in red). If the trace has a linked profile (see [Profiling](/docs/profiling)), a link to its flame graph appears above the waterfall — a way to go from "which span was slow" to "what exactly was running inside it."

## Web Vitals

Web Vitals are page-speed metrics measured in real users' browsers (RUM), not by a synthetic server-side check. The section opens via the **"Web Vitals"** tab in the "Performance" subsection menu — URL `/projects/<id>/web-vitals`.

| Metric | What it measures |
|---|---|
| **LCP** (Largest Contentful Paint) | when the largest visible element finished rendering — the "page has loaded" feeling |
| **INP** (Interaction to Next Paint) | responsiveness to user actions (click, tap, keypress) over the page's whole lifetime |
| **CLS** (Cumulative Layout Shift) | total layout "jumpiness" — how much elements shift unexpectedly |
| **FCP** (First Contentful Paint) | when the first content rendered |
| **TTFB** (Time to First Byte) | time to the first byte of the server's response |

### Thresholds (Google, fixed)

The good / needs-improvement / poor rating is computed from the 75th percentile (p75) of the value over the selected period — these are Google's official Core Web Vitals thresholds. They are **not configurable per project**: the same numbers apply to everyone, because this is an external, industry-standard bar, not a Gotcha-specific metric.

| Metric | Good (p75 ≤) | Poor (p75 >) | Needs improvement |
|---|---|---|---|
| LCP | 2500 ms | 4000 ms | in between |
| INP | 200 ms | 500 ms | in between |
| CLS | 0.10 | 0.25 | in between |
| FCP | 1800 ms | 3000 ms | in between |
| TTFB | 800 ms | 1800 ms | in between |

The good boundary is inclusive (a p75 equal to the threshold is still "good"), a value above the poor threshold is "poor," everything in between is "needs-improvement."

### How p75 is computed

For each page (page-load transaction) and metric, Gotcha takes the 75th percentile of the values collected over the selected filter period (1 hour / 24 hours / 7 days / 30 days) and assigns a rating from the table above. If there are no samples for a metric in the period, "—" is shown instead of a value, with no badge.

### Reading the page

The table has one row per page (transaction), with p75 LCP/INP/CLS columns showing a color-coded rating badge and a sample count; sortable by clicking a column header, with the same environment and period filters as the endpoint list. On a given endpoint's page (if it's a page-load transaction), the same three metrics appear as a panel with a small p75-over-time chart — a quick way to see whether it recently got worse or has been stable.

The data comes from the browser SDK (`@sentry/browser`) with tracing enabled — without it, there's nothing collecting Web Vitals; server-side SDKs don't do this. Installation is covered in [SDK and integrations](/docs/sdk).

## Regressions

A **performance regression** is a statistically significant degradation in an endpoint's p95 duration or a web vital's p75, relative to a rolling baseline, detected automatically — without hand-tuning a threshold "by eye" for every endpoint. The list lives in the **"Regressions"** subsection (`/projects/<id>/regressions`), with "Open / Resolved / All" tabs. The table shows: target (an endpoint or a link to Web Vitals), metric, percentage increase, "baseline → peak" range, status, when it started, and duration (or "ongoing" while the regression is open).

### How detection works (threshold + hysteresis)

A background evaluator periodically compares a recent window (60 minutes by default) against a baseline — the median of daily values over the last 7 days — for the project's highest-traffic endpoints and web-vitals pages:

- **Opening** only happens when two conditions both hold: a relative increase (`recent > baseline × (1 + threshold)`) **and** an absolute floor (`recent > baseline + floor`). The default threshold is 25%. The floor exists so that going from 20 ms to 40 ms (a nominal +100%) on numbers nobody would notice doesn't trigger a false alarm.
- **Closing** uses a looser recovery threshold — 10% by default (`recent ≤ baseline × 1.10`), not the same 25% used to open. This is the hysteresis: without a gap between the opening and closing thresholds, a regression would flicker open/closed right at the boundary.
- A decision is only made when there's enough statistics: both the recent window and the baseline need at least a minimum number of samples (100 by default), otherwise the evaluator makes no decision that tick.

Default absolute floors: 100 ms for endpoint duration, 200 ms for LCP/FCP/TTFB, 50 ms for INP, 0.05 for CLS.

All of these thresholds (opening/closing percentages, window size, minimum samples, per-metric floors, and a full on/off switch for the detector) are configurable per project — the **"Regressions"** card on the **"Project settings"** page.
