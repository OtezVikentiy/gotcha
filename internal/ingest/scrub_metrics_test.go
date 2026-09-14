package ingest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/scrub"
)

type stubKeyResolver struct{ key org.Key }

func (r stubKeyResolver) KeyByPublic(_ context.Context, _ string) (org.Key, error) {
	return r.key, nil
}

type collectMetricSink struct{ points []metric.MetricPoint }

func (s *collectMetricSink) Add(_ int64, p metric.MetricPoint) {
	s.points = append(s.points, p)
}

func TestOTLPMetricsScrubAttributes(t *testing.T) {
	sink := &collectMetricSink{}
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}}), nil, nil, 1<<20)
	h.Metrics = sink
	h.Scrub = scrub.NewScrubber(true, false, []string{"token"})

	md := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "cpu", Unit: "1", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{{
					TimeUnixNano: uint64(time.Now().Add(-time.Hour).UnixNano()),
					Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 0.5},
					Attributes: []*commonpb.KeyValue{
						{Key: "token", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "secret"}}},
						{Key: "name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "cpu"}}},
					},
				}},
			}}},
		}}},
	}}}
	raw, err := proto.Marshal(md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/metrics", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer pub")
	w := httptest.NewRecorder()
	h.otlpMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(sink.points) != 1 {
		t.Fatalf("принято точек = %d, want 1", len(sink.points))
	}
	got := sink.points[0]
	if got.Attributes["token"] != "[scrubbed]" {
		t.Errorf("attributes[token] = %q, want %q", got.Attributes["token"], "[scrubbed]")
	}
	if got.Attributes["name"] != "cpu" {
		t.Errorf("attributes[name] = %q, want не тронут", got.Attributes["name"])
	}
}

func TestOTLPMetricsHostCardinalityCollapses(t *testing.T) {
	sink := &collectMetricSink{}
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindLegacy}}), nil, nil, 1<<20)
	h.Metrics = sink
	h.Cardinality = NewCardinalityGuard(1, time.Hour)
	h.Cardinality.Value(1, FieldHost, "web-1")

	md := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "host.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "web-2"}}},
		}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "cpu", Unit: "1", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{{
					TimeUnixNano: uint64(time.Now().Add(-time.Hour).UnixNano()),
					Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 0.5},
				}},
			}}},
		}}},
	}}}
	raw, err := proto.Marshal(md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/metrics", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer pub")
	w := httptest.NewRecorder()
	h.otlpMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(sink.points) != 1 {
		t.Fatalf("принято точек = %d, want 1", len(sink.points))
	}
	if got := sink.points[0].Host; got != CardinalityOverflow {
		t.Errorf("host = %q, want %q (потолок исчерпан)", got, CardinalityOverflow)
	}
}
