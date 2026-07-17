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
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// stubKeyResolver отдаёт фиксированный ключ на любой public key — авторизация
// OTLP-входа без PG.
type stubKeyResolver struct{ key org.Key }

func (r stubKeyResolver) KeyByPublic(_ context.Context, _ string) (org.Key, error) {
	return r.key, nil
}

// collectMetricSink копит принятые metric-точки для проверки скраба атрибутов.
type collectMetricSink struct{ points []metric.MetricPoint }

func (s *collectMetricSink) Add(_ int64, p metric.MetricPoint) {
	s.points = append(s.points, p)
}

// TestOTLPMetricsScrubAttributes: атрибуты OTLP-датапойнта проходят через
// scrubber перед записью — denylist-ключ заменён маской, прочие атрибуты целы.
func TestOTLPMetricsScrubAttributes(t *testing.T) {
	sink := &collectMetricSink{}
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1}}), nil, nil, 1<<20)
	h.Metrics = sink
	h.Scrub = NewScrubber(true, false, []string{"token"})

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
	if got.Attributes["token"] != scrubMask {
		t.Errorf("attributes[token] = %q, want %q", got.Attributes["token"], scrubMask)
	}
	if got.Attributes["name"] != "cpu" {
		t.Errorf("attributes[name] = %q, want не тронут", got.Attributes["name"])
	}
}
