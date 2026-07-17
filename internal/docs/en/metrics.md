# Metrics

The "Metrics" section stores numeric time series that your application sends over the **OTLP** (OpenTelemetry Protocol) protocol. This is a separate ingest channel from errors and transactions: a metric is a number (e.g. request duration, queue size, order count) measured at a point in time, optionally tagged with labels (environment, region, status code, etc.).

Open it via the chart icon in the left icon rail ("Metrics") or directly at `/projects/{id}/metrics`.

## How to send a metric

### Ingest endpoint

Gotcha accepts metrics over OTLP/HTTP at:

```
POST /v1/metrics
```

on the same host as the rest of event ingest (see your project's DSN). Both standard OTLP encodings are supported:

| `Content-Type`                      | Body format               |
|--------------------------------------|----------------------------|
| `application/x-protobuf` (or `application/protobuf`) | Binary protobuf — what an OTel exporter sends by default |
| `application/json`                   | OTLP/JSON — the same protocol, JSON-encoded |

### Authentication

Like error/transaction ingest, `/v1/metrics` is authorized with the project's public key — not in the URL, but in a header:

```
Authorization: Bearer <PUBLIC_KEY>
```

`<PUBLIC_KEY>` is the part of the project DSN between `https://` and `@`. Get the DSN from the project's **"Setup"** page (see [SDK & integrations](/docs/sdk)): for a DSN like

```
https://a1b2c3d4e5f6@gotcha.example.com/42
```

the key is `a1b2c3d4e5f6`, and the host for metrics is `gotcha.example.com`. The project is resolved from the key, so `/v1/metrics` doesn't carry a project id in the path. A missing or invalid key returns `401`.

### Quick check with curl

The simplest way to send a test point is OTLP/JSON, no SDK required:

```bash
curl -X POST https://gotcha.example.com/v1/metrics \
  -H "Authorization: Bearer a1b2c3d4e5f6" \
  -H "Content-Type: application/json" \
  -d '{
    "resourceMetrics": [{
      "resource": {
        "attributes": [
          {"key": "service.name", "value": {"stringValue": "my-php-app"}},
          {"key": "deployment.environment.name", "value": {"stringValue": "production"}}
        ]
      },
      "scopeMetrics": [{
        "metrics": [{
          "name": "http.server.duration",
          "unit": "ms",
          "gauge": {
            "dataPoints": [{
              "asDouble": 123.4,
              "timeUnixNano": "1700000000000000000",
              "attributes": [{"key": "route", "value": {"stringValue": "/checkout"}}]
            }]
          }
        }]
      }]
    }]
  }'
```

If the key is valid and metric ingest is enabled on the instance, the response is an empty `200 OK`. Within a minute or two, `http.server.duration` shows up in the metrics list.

### Configuring an OTel exporter with environment variables

For regular traffic, it's easier to configure your language's OTel SDK/exporter (Go, PHP, Node, Python — any SDK that honors the standard OTLP environment variables) instead of building JSON by hand:

```bash
OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=https://gotcha.example.com/v1/metrics
OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_METRICS_HEADERS=Authorization=Bearer%20a1b2c3d4e5f6
OTEL_METRIC_EXPORT_INTERVAL=15000
```

For PHP that's typically the `open-telemetry/exporter-otlp` package on top of `open-telemetry/sdk` — it reads these same variables with no extra code. The exporter builds the `resourceMetrics`/`scopeMetrics` payload itself and flushes it on a timer (`OTEL_METRIC_EXPORT_INTERVAL`, milliseconds).

> Metrics are not sampled and don't depend on the tracing toggle — if metric ingest is enabled on the instance, every point is accepted (within the organization's quota).

## Which metric types are accepted

| OTLP type | What it is | Aggregations available in the UI |
|---|---|---|
| **Sum** (counter) | A monotonically increasing counter (`isMonotonic: true`, `aggregationTemporality: CUMULATIVE`) — e.g. orders processed. Shown on the chart as a **rate**: the difference between consecutive points divided by the bucket step (value per second). A non-monotonic or delta Sum is shown as-is, with the regular aggregations. | avg / max / min / sum (rate is automatic for a monotonic cumulative Sum) |
| **Gauge** | An instantaneous point-in-time value — e.g. queue depth, open connections. | avg / max / min / sum |
| **Histogram** | A distribution of values across buckets (`bucketCounts` + `explicitBounds`) — e.g. request duration. | p50 / p95 / p99 (interpolated within the bucket) / avg (mean observation = sum/count) |

`ExponentialHistogram` and `Summary` are not supported yet — such metrics are silently skipped during parsing.

`NaN`/`±Inf` values in datapoints are dropped at ingest rather than stored as-is.

## Labels and environment

- `service.name` from the resource attributes becomes the metric's "service".
- `deployment.environment.name` (current OTel semantic convention) or `deployment.environment` (legacy) becomes the **environment**, used to filter the list and the chart.
- The remaining datapoint attributes (e.g. `route`, `status_code`, `region`) become the metric's **labels** — you can filter the detail chart by them and jump between slices.

Up to 64 labels are accepted per point (extras are dropped deterministically, by sorted key); the UI shows up to 20 known values per label key.

## Ingest window

A point timestamped more than 90 days in the past, or more than a day in the future relative to ingest time, is dropped (the same ClickHouse partition guard used for events/traces) — an exporter with badly skewed clocks will lose points.

## Metrics list

`/projects/{id}/metrics` lists every metric in the project: name, type (`gauge`/`sum`/`histogram`), unit. You can filter by environment at the top. While there are no metrics yet, the page shows a hint linking back to this guide.

## Metric detail page

Clicking a metric name opens `/projects/{id}/metrics/{name}` — a time series chart with filters:

- **Period** — `1h` / `24h` / `7d` (the bucket step adapts: a minute / 10 minutes / an hour);
- **Aggregation** — the choices depend on the metric type (see the table above: histograms default to percentiles, everything else to avg/max/min/sum);
- **Environment** — from the environments known for this metric, "all" by default.

The chart is an SVG with labeled axes (Y-axis values are formatted using the metric's unit). If an enabled [metric alert rule](/docs/metric-alerts) exists for this metric with the same aggregation, its threshold is drawn as a horizontal **dashed line** labeled with the condition (e.g. `> 500`) — so you can see at a glance how close the current curve is to firing.

Below the chart is the list of known labels; clicking a label value reopens the same chart filtered by that label (`label_key`/`label_value` in the URL).

## Settings and quotas

There's currently no dedicated per-project metrics settings page (unlike, say, span retention under Performance) — metrics are retained under the instance-wide policy. Metric ingest counts against the organization's monthly quota; the operator sets the default via `GOTCHA_DEFAULT_METRIC_QUOTA` (see [Configuration](/docs/configuration)) and it can be fine-tuned under "Organization settings → Usage & rate limits". Once the quota is exhausted, `/v1/metrics` returns `429`; already-ingested points are not deleted.

## Alerts on metrics

You can attach a rule to a metric: a threshold on an aggregate value over a time window that, when crossed, opens an incident and sends a notification to the project's channels. See [Metric Alerts](/docs/metric-alerts) for the full walkthrough with an example, and [Alerts](/docs/alerts) for the delivery channels themselves.
