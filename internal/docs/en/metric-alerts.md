# Metric Alerts

A metric alert rule watches a metric's aggregate (avg/max/p95, etc.) over a rolling time window and opens an **incident** when the value crosses a threshold. Each open and each close sends exactly one notification to the project's delivery channels — the same email/webhook/Telegram channels used for issue alerts (see [Alerts](/docs/alerts)).

## Where to find it

The chart icon in the left icon rail → "Metrics" → the "Metric Alerts" sub-item (or go directly to `/projects/{id}/metrics/alerts`). Only the project's owner and admins can manage rules.

## Creating a rule

The **"New rule"** button opens a form:

| Field | What to enter |
|---|---|
| Metric | The exact metric name as it arrives over OTLP (e.g. `http.server.duration`) — case and spelling must match exactly |
| Aggregation | `avg`, `max`, `min`, `sum`, `p50`, `p95`, `p99` |
| Condition | `>` (greater than) or `<` (less than) |
| Threshold | The number the aggregate is compared against. Must be finite — `NaN`/`Infinity` are rejected with "threshold must be a finite number" |
| Window (s) | Width of the rolling window in seconds, a positive integer (e.g. `300` for 5 minutes) |
| Environment | Optional; exact match. Empty = any environment |
| Label key / Label value | Optional extra filter on a single metric label (exact match); fill in both or neither |

### Available aggregations

| Aggregation | Meaning |
|---|---|
| `avg` | Average value over the window |
| `max` | Maximum over the window |
| `min` | Minimum over the window |
| `sum` | Sum of values over the window |
| `p50` / `p95` / `p99` | Percentile (only meaningful for histogram-type metrics; other types won't produce a meaningful percentile) |

### Conditions

| Condition | Symbol | Breach | Recovery (closes the incident) |
|---|---|---|---|
| gt | `>` | current value `>` threshold | current value `≤` threshold × 0.95 |
| lt | `<` | current value `<` threshold | current value `≥` threshold × 1.05 |

Recovery deliberately requires the value to move 5% past the threshold on the safe side (hysteresis) — otherwise a value oscillating right at the boundary would open and close the incident on every check.

## How it's evaluated

A background evaluator sweeps every enabled rule **once a minute**. On each pass, it computes the metric's aggregate over the window `[now − window, now)` — the same query that draws the chart on the metric detail page. If there's no data at all for the window, no decision is made (the incident is neither opened nor closed; it waits for the next pass).

Then:
- no open incident and the value breaches the threshold → an **incident opens**, a "firing" notification is sent;
- an incident is already open and the value is still breaching (or sitting in the hysteresis dead zone) → the incident is updated (current value, and peak — the worst value seen during the incident); the notification is **not** repeated;
- an incident is open and the value has recovered (see the table above) → the **incident closes**, a "resolved" notification is sent.

At most one incident can be open per rule at any time.

## Incidents

Below the rules table, on the same page, is the project's incident list (the last 100): status (**Open** / **Resolved**), the peak and current aggregate value, and the start time. The empty state notes that an incident will show up here as soon as a metric crosses a rule's threshold.

## Notifications and channels

Open/close notifications are queued to **every enabled delivery channel in the project** — the same channels configured under [Alerts](/docs/alerts) (email/webhook/Telegram); there's no separate channel setup for metrics. Email is skipped with a warning in the log if SMTP isn't configured (see [Configuration](/docs/configuration)). External channels (webhook/Telegram) respect the same `GOTCHA_EXTERNAL_CHANNEL_DETAILS` privacy setting used for issue alerts: when it's off, only a link to the rules page and the event kind go out — no metric name or values.

## The threshold on the chart

If the metric detail page (`/projects/{id}/metrics/{name}`) has the same aggregation selected as an enabled rule, that rule's threshold is drawn as a horizontal **dashed line** labeled with the condition (e.g. `p95 > 500`). See [Metrics](/docs/metrics) for more on the chart itself.

## Worked example: alert when p95 latency > 500ms for 5 minutes

Say your app sends a histogram metric `http.server.duration` (unit `ms`) — see [Metrics](/docs/metrics) for how to send it over OTLP.

1. Open `/projects/{id}/metrics/http.server.duration` to confirm points are arriving, and double-check the exact metric name and unit.
2. Go to "Metrics → Metric Alerts" and click **"New rule"**.
3. Fill in the form:
   - Metric: `http.server.duration`
   - Aggregation: `p95`
   - Condition: `>`
   - Threshold: `500`
   - Window (s): `300` (that's 5 minutes)
   - Environment: `production` (optional, but useful — otherwise the window mixes production with local dev traffic)
   - Label key/value — leave blank
4. Click "Create rule". The table now shows `http.server.duration | p95 > 500 | 300s | production | Enabled`.
5. Once the trailing-5-minute p95 exceeds 500 (in whatever unit the metric carries — Gotcha doesn't convert units), an incident opens and a notification goes out to the project's enabled channels. A dashed line at 500 appears on the metric chart when viewing the p95 aggregation.
6. When p95 drops back below 475 (500 × 0.95), the incident closes and a second, "resolved" notification is sent.

## Deleting a rule

The "Delete" button on the rule's row removes it immediately (no confirmation step). Past or open incidents already recorded for that rule stay in the incident history.

## See also

- [Metrics](/docs/metrics) — what a metric is, how to send one, the detail chart.
- [Alerts](/docs/alerts) — delivery channels, issue alerts, and the failed-delivery log.
