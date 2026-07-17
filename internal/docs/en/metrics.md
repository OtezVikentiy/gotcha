# Metrics

The "Metrics" section stores numeric time series that your application sends via the SDK or over the OTLP protocol.

## Types and aggregations
A metric can be a counter (only increases), a gauge (an arbitrary point-in-time value), or a histogram (a distribution of values). Available aggregations for viewing are p50/p95/avg/sum over the selected period.

## Filters and labels
Each metric can carry labels (for example, environment, region, release version). The metrics list can be filtered by environment, period, and label values, letting you compare the same measurement across different slices.

## Alerts on metrics
You can attach an alert rule to a metric — a threshold on an aggregation's value over an interval; when exceeded, the delivery channel configured in "Alerts" is triggered.
