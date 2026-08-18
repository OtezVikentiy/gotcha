# Logs

The telemetry channel for structured application logs: the ingest API
(described below) and a browsing/search screen in the UI.

## Browsing and search

The project's logs screen lives at `/projects/{id}/logs` ("Logs" in the
project navigation). The list shows the newest records first; clicking a row
expands the full body, a table of attributes (`log_attributes` and
`resource_attrs` shown separately), and `trace_id`/`span_id` when the record
carries them.

### Filters

The filter form above the list needs no JavaScript (a plain GET form —
"Apply" reloads the page with the new filter):

- **Time range** — the product's shared [time range control](/docs/time-range)
  (presets 1h/24h/7d/30d plus a custom range). For logs, the window is also
  clamped to the retention period (`GOTCHA_LOG_RETENTION_DAYS`, 14 days by
  default — see [Configuration](/docs/configuration)): picking a preset wider
  than the actual TTL yields a shorter effective window, since a query past
  retention would return nothing anyway. The screen makes this explicit — a
  "window clamped to the log retention period (N days)" note next to the
  filters, not a silently shorter list.
- **Severity** — a multi-select over the six canonical levels (`trace` …
  `fatal`, see [canonicalization](#what-gets-canonicalized-in-severity)
  below).
- **Service** and **Environment** — exact match against the values attached
  at ingest (`service.name`/`deployment.environment.name` from OTLP).
- **Text search** — a case-insensitive substring search over the record body.

Active filters are reflected in the URL — the current slice can be shared or
the page reloaded without losing state.

### Facets

The sidebar next to the list shows **facets** — top values with record counts
for the current window and the rest of the active filters:

- **Severity, service, environment** — the same three dimensions as the
  filter form; clicking a value adds it to the filter (or removes it, if
  already selected). The severity facet excludes its own filter from the
  count — it always shows the distribution across all levels, even with one
  or two already selected (otherwise a selected level would immediately
  swallow the whole facet).
- **Attributes** — keys auto-discovered in `log_attributes` across the
  current window's records (top by frequency), each with a count. Clicking a
  key expands its top 10 values (also with counts) — the server computes
  those lazily, only for the expanded key, not for every key at once.
  Clicking a value adds a pinpoint filter on that attribute (see below).

If a facet can't finish computing in time (a very wide window plus a lot of
data), it shows "too much data" instead of values — narrow the window or add
a filter. This doesn't fail the whole page: the list and the other facets
keep working.

### Attribute key autocomplete

The attribute search field in the sidebar suggests keys as you type
(typeahead): start typing a prefix (e.g. `http.`) and matching keys appear
(`http.method`, `http.status_code`, …) with their frequency. It searches the
same time window as the current list filter (the preset/custom range above),
not a separate fixed window — suggestions match what's visible in the
sidebar and the list. This is a JS enhancement on top of the "Attributes"
sidebar facet, not a separate form: without JavaScript the field itself is
inactive (it submits nothing), but the keys are still available — listed
with their counts right in the sidebar, each expandable by click into its
top values (see above).

### Pinpoint attribute filters

Besides clicking a facet, a slice on a specific attribute can be set manually
via the repeatable `attr` query parameter:

```
/projects/{id}/logs?attr=http.method:GET&attr=http.status_code:500
```

The value is `key:value`, split on the first `:` (the value itself may
contain a colon, e.g. a URL: `attr=http.url:http://example.com/x`). To filter
on a **resource** attribute (`resource_attrs`, not `log_attributes`), prefix
with `res:`: `attr=res:host.name:web-01`. Multiple `attr` parameters narrow
the result (logical AND). Only exact match is supported — regular
expressions and "not equal" are out of scope for the MVP.

### Volume histogram

Above the list, a histogram shows record volume over time, broken down by
severity (the same stacked style as the rest of the product's charts). Bucket
width is picked to fit the selected window's span. If there's no data for the
current filter, the histogram is hidden instead of rendering an empty chart.

### Pagination

The list shows one page of recent records; the **"Show older"** button loads
the next page of the same size via a time-based cursor — there's no "total
found" count or deep page navigation (at log volume, offset pagination with a
total count would be an expensive full scan). Getting an accurate picture of
the whole period is better done through facets and the histogram than by
paging through the list to the end.

## How to send a log

Two independent formats are accepted on separate paths of the same host (see
your project's DSN) — pick whichever fits your stack better.

### Authentication

Like `/v1/metrics`, both log ingest endpoints are authorized with the
project's public key in a header:

```
Authorization: Bearer <PUBLIC_KEY>
```

`<PUBLIC_KEY>` is the part of the project DSN between `https://` and `@` (see
the project's "Setup" page and [SDK & integrations](/docs/sdk)). A missing
or invalid key returns `401`.

### OTLP: `POST /v1/logs`

For clients with an OTel SDK/collector — the same protocol used by
[metrics](/docs/metrics) and traces. Both standard OTLP encodings are
supported:

| `Content-Type` | Body format |
|---|---|
| `application/x-protobuf` (or `application/protobuf`) | Binary protobuf — what an OTel exporter sends by default |
| `application/json` | OTLP/JSON — the same protocol, JSON-encoded |

An OTLP/JSON example, no SDK required:

```bash
curl -X POST https://gotcha.example.com/v1/logs \
  -H "Authorization: Bearer a1b2c3d4e5f6" \
  -H "Content-Type: application/json" \
  -d '{
    "resourceLogs": [{
      "resource": {
        "attributes": [
          {"key": "service.name", "value": {"stringValue": "my-php-app"}},
          {"key": "deployment.environment.name", "value": {"stringValue": "production"}}
        ]
      },
      "scopeLogs": [{
        "logRecords": [{
          "timeUnixNano": "1700000000000000000",
          "severityNumber": 17,
          "severityText": "ERROR",
          "body": {"stringValue": "payment failed: gateway timeout"},
          "attributes": [{"key": "order_id", "value": {"stringValue": "42"}}],
          "traceId": "5b8aa5a2d2c872e8321cf37308d69df2",
          "spanId": "051581bf3cb55c13"
        }]
      }]
    }]
  }'
```

On success the response is an empty `200 OK` (the standard empty OTLP
envelope). `service.name`/`deployment.environment.name` (or the legacy
`deployment.environment`) from the resource attributes become the record's
"service" and "environment", the same as metrics; `traceId`/`spanId` are
stored as-is (hex) — groundwork for log↔trace correlation in a future
release.

The record text (`body`) goes through the same unconditional URL scrub as
error messages: query-string tokens and basic-auth in URLs inside the text
are always stripped, regardless of `GOTCHA_SCRUB_FREETEXT` (see
[Privacy](/docs/privacy)) — privacy by default, same as the rest of the
telemetry.

For regular traffic it's easier to configure your OTel SDK's log exporter
with environment variables, the same way as metrics:

```bash
OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://gotcha.example.com/v1/logs
OTEL_EXPORTER_OTLP_LOGS_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_LOGS_HEADERS=Authorization=Bearer%20a1b2c3d4e5f6
```

### NDJSON: `POST /logs`

A simpler path for sources without an OTel exporter — an ad hoc script, a log
shipping agent, a manual `curl`. The body is newline-delimited JSON: one
record per line, no array wrapper.

Line schema:

| Field | Required | Description |
|---|---|---|
| `message` | required | The record's text. An empty or missing value — the whole line is skipped (it doesn't fail the rest of the batch). |
| `level` | optional | A text severity (`info`, `warn`, `err`, `critical`, etc. — see canonicalization below). Empty or unrecognized → `info`. |
| `timestamp` | optional | Either an RFC3339 string (`"2026-08-18T12:00:00Z"`) or unix time in seconds as a number (a fraction is allowed, `1755518400.5`). Missing, empty, unparsable, or not later than the epoch — the server's receive time is used instead. |
| `attributes` | optional | An arbitrary JSON object: string/number/bool values are copied as-is, nested objects/arrays are serialized back to a JSON string. |
| `trace_id` | optional | A string (no format validation, unlike OTLP), capped at 64 characters — longer values are truncated, same as other fields. |
| `span_id` | optional | Same. |

NDJSON records carry no resource attributes — their "service" and
"environment" fields are left unset (this may change in a future release);
if you need that attribution today, use OTLP instead.

Example:

```bash
curl -X POST https://gotcha.example.com/logs \
  -H "Authorization: Bearer a1b2c3d4e5f6" \
  --data-binary $'{"message":"payment failed: gateway timeout","level":"error","attributes":{"order_id":42}}\n{"message":"retrying in 5s","level":"info"}\n'
```

On success the response is `200 OK` with the body `{"accepted": N}`, where
`N` is the number of records actually granted quota and stored (it can be
smaller than the number of lines in the body if some were rejected by quota
— see below). A line that doesn't parse as JSON, or has an empty `message`,
is silently skipped and doesn't count toward `N`, but doesn't fail the rest
of the batch.

## What gets canonicalized in severity

The UI and alert rules (once they exist) work off a single six-level canon
that any source is reduced to:

`trace`, `debug`, `info`, `warn`, `error`, `fatal`

**From OTLP `SeverityNumber`** (1–24 per the OTel spec): 1–4 → `trace`, 5–8 →
`debug`, 9–12 → `info`, 13–16 → `warn`, 17–20 → `error`, 21–24 → `fatal`. A
number outside this range (0, negative, >24) isn't a format error — the
record isn't dropped, it just gets a neutral `info`.

**From text** (NDJSON `level`, and also OTLP `SeverityText` — but only as a
fallback when `SeverityNumber` is unset): `trace`; `debug`; `info`;
`warn`/`warning`; `error`/`err`; `fatal`/`critical` (case-insensitive). A
numeric string (`"17"`) is treated as `SeverityNumber`. Empty or
unrecognized text also becomes `info`.

The raw `severity_number`/`severity_text` are kept alongside the canonical
`severity` — for debugging and audit, not just the canon.

## Limits

- **Up to 10,000 records** per request (both OTLP and NDJSON) — extras
  within the request are dropped without failing the whole batch.
- **Up to 64 KiB** per record's text (`body`/`message`) — longer text is
  truncated.
- NDJSON: a line longer than 256 KiB is dropped without being parsed (and
  doesn't fail the rest of the request).
- **Up to 64 attributes** per record, key up to 64 characters, value up to
  200; on overflow, the first 64 by sorted key are kept (deterministic, not
  random).
- The overall request body is capped by the same variable used for the rest
  of ingest, `GOTCHA_MAX_EVENT_BYTES` (see [Configuration](/docs/configuration))
  — a body larger than the cap is rejected with `413`.
- Exceeding the per-DSN rate limit, or running out of quota, returns `429`.

## Ingest window

A record timestamped more than 90 days in the past, or more than a day in
the future relative to ingest time, is clamped to the edge of that window
(not dropped outright, unlike metrics — the body and severity remain useful
even if the client's clock has drifted).

## Storage, quota, and settings

Logs are stored in ClickHouse under the instance-wide policy; there's no
dedicated per-project settings page yet. The operator sets the retention
period via `GOTCHA_LOG_RETENTION_DAYS` (default 14 days — logs are more
voluminous than events, hence the shorter default; `0` keeps data forever).
Log ingest counts against the organization's monthly quota: the default
limit is `GOTCHA_DEFAULT_LOG_QUOTA`, fine-tuned under "Organization settings
→ Usage & rate limits" (see [Configuration](/docs/configuration)). Once the
quota is exhausted, `/v1/logs` and `/logs` return `429`; already-ingested
records are not deleted.
