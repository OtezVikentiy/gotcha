# Performance

The "Performance" section shows your application's endpoints with latency percentiles (p50/p75/p95/p99), traffic, and the Apdex index.

## Endpoints and Apdex
The endpoint list can be sorted by traffic or latency; Apdex measures the share of requests that finish within a satisfactory response-time threshold T. A sharp rise in p95/p99 with steady traffic usually points to a degradation rather than a load spike.

## Endpoint detail
The endpoint page shows a latency histogram, a chart over time, and a list of the slowest traces, with a link to related issues when a request ended in an error.

## Web Vitals
A separate tab shows browser page-speed metrics measured in the user's browser — LCP, INP, CLS, FCP, TTFB — with the p75 percentile and a good/needs-improvement/poor rating for each page.

## Regressions
Performance issues are automatically detected regressions: a statistically significant degradation in latency or Web Vitals relative to a baseline from the previous period.
