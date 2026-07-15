package ingest_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// postMetrics шлёт тело на /v1/metrics; bearer == "" → без Authorization.
func (s *stack) postMetrics(t *testing.T, body []byte, contentType, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", s.srv.URL+"/v1/metrics", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestOTLPMetricsEndpoint(t *testing.T) {
	s := newStack(t)

	md := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api"}}},
		}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "cpu", Unit: "1", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{{
					TimeUnixNano: uint64(time.Now().UnixNano()),
					Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 0.5},
				}},
			}}},
		}}},
	}}}
	raw, err := proto.Marshal(md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// proto → 200, точка принята.
	resp := s.postMetrics(t, raw, "application/x-protobuf", s.key.PublicKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/metrics status = %d, want 200", resp.StatusCode)
	}
	if s.metrics.count() != 1 {
		t.Fatalf("sink received %d points, want 1", s.metrics.count())
	}

	// Без ключа → 401.
	resp = s.postMetrics(t, raw, "application/x-protobuf", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-key status = %d, want 401", resp.StatusCode)
	}

	// Неподдерживаемый content-type → 415.
	resp = s.postMetrics(t, raw, "text/plain", s.key.PublicKey)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content-type status = %d, want 415", resp.StatusCode)
	}
}
