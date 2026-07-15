package metric

import (
	"math"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: v},
	}}
}

func numDP(val float64, attrs ...*commonpb.KeyValue) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{
		Attributes:   attrs,
		TimeUnixNano: uint64(time.Unix(1600000000, 0).UnixNano()),
		Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: val},
	}
}

func gaugeMetric(name, unit string, val float64, attrs ...*commonpb.KeyValue) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Unit: unit, Data: &metricspb.Metric_Gauge{
		Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{numDP(val, attrs...)}},
	}}
}

func sumMetric(name, unit string, val float64, mono bool, temp metricspb.AggregationTemporality) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Unit: unit, Data: &metricspb.Metric_Sum{
		Sum: &metricspb.Sum{
			DataPoints:             []*metricspb.NumberDataPoint{numDP(val)},
			IsMonotonic:            mono,
			AggregationTemporality: temp,
		},
	}}
}

func histMetric(name, unit string, count uint64, sum float64, buckets []uint64, bounds []float64) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Unit: unit, Data: &metricspb.Metric_Histogram{
		Histogram: &metricspb.Histogram{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			DataPoints: []*metricspb.HistogramDataPoint{{
				TimeUnixNano:   uint64(time.Unix(1600000000, 0).UnixNano()),
				Count:          count,
				Sum:            &sum,
				BucketCounts:   buckets,
				ExplicitBounds: bounds,
			}},
		},
	}}
}

func TestMapOTLPGaugeSumHistogram(t *testing.T) {
	rm := []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			kv("service.name", "api"), kv("deployment.environment", "prod"),
		}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			gaugeMetric("mem.usage", "By", 42.0, kv("host", "h1")),
			sumMetric("http.requests", "1", 100.0, true, metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE),
			histMetric("http.duration", "ms", 12, 240.0, []uint64{2, 8, 2}, []float64{100, 500}),
		}}},
	}}
	points := MapOTLP(rm, time.Now())
	if len(points) != 3 {
		t.Fatalf("points = %d, want 3", len(points))
	}
	byName := map[string]MetricPoint{}
	for _, p := range points {
		byName[p.Name] = p
	}
	g := byName["mem.usage"]
	if g.Type != "gauge" || g.Value != 42.0 || g.Service != "api" || g.Environment != "prod" || g.Attributes["host"] != "h1" {
		t.Fatalf("gauge = %+v", g)
	}
	s := byName["http.requests"]
	if s.Type != "sum" || !s.Monotonic || s.Temporality != "cumulative" {
		t.Fatalf("sum = %+v", s)
	}
	h := byName["http.duration"]
	if h.Type != "histogram" || h.Count != 12 || h.Value != 240.0 ||
		len(h.BucketCounts) != 3 || len(h.ExplicitBounds) != 2 {
		t.Fatalf("histogram = %+v", h)
	}
}

func TestMapOTLPDropsNaN(t *testing.T) {
	rm := []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			gaugeMetric("bad", "1", math.NaN()),
			gaugeMetric("good", "1", 1.0),
		}}},
	}}
	points := MapOTLP(rm, time.Now())
	if len(points) != 1 || points[0].Name != "good" {
		t.Fatalf("NaN not dropped: %+v", points)
	}
}

func TestMapOTLPSkipsUnsupported(t *testing.T) {
	// Summary — вне объёма, пропускается.
	rm := []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "sum.summary", Data: &metricspb.Metric_Summary{Summary: &metricspb.Summary{
				DataPoints: []*metricspb.SummaryDataPoint{{Count: 1}},
			}}},
		}}},
	}}
	if points := MapOTLP(rm, time.Now()); len(points) != 0 {
		t.Fatalf("summary must be skipped, got %+v", points)
	}
}

func TestMapOTLPFallbackTS(t *testing.T) {
	fb := time.Unix(1234567890, 0).UTC()
	rm := []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: "g", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{{
					Value: &metricspb.NumberDataPoint_AsInt{AsInt: 5}, // TimeUnixNano=0
				}},
			}},
		}}}},
	}}
	points := MapOTLP(rm, fb)
	if len(points) != 1 || !points[0].TS.Equal(fb) || points[0].Value != 5 {
		t.Fatalf("fallback ts/int value = %+v", points)
	}
}
