package metric

import (
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// TestMapOTLPCapsPoints — потолок числа датапойнтов на один OTLP /v1/metrics
// запрос: свыше maxOTLPMetricPoints лишнее отбрасывается (защита от
// амплификации памяти/CPU недоверенным экспортом).
func TestMapOTLPCapsPoints(t *testing.T) {
	now := time.Now().UTC()
	pts := make(map[uint64]float64, maxOTLPMetricPoints+100)
	for i := 0; i < maxOTLPMetricPoints+100; i++ {
		// уникальные валидные (в окне) таймстемпы
		ts := uint64(now.Add(-time.Duration(i) * time.Second).UnixNano())
		pts[ts] = float64(i)
	}
	rm := gaugeResourceMetrics(t, pts)
	out := MapOTLP(rm, now)
	if len(out) != maxOTLPMetricPoints {
		t.Fatalf("MapOTLP capped len = %d, want %d", len(out), maxOTLPMetricPoints)
	}
}

// TestMapOTLPCapsHistogramBuckets — потолок длины массивов гистограммы: экспорт с
// гигантскими bucket_counts/explicit_bounds обрезается до maxHistogramBuckets
// (защита от амплификации памяти/записи недоверенным экспортом).
func TestMapOTLPCapsHistogramBuckets(t *testing.T) {
	buckets := make([]uint64, maxHistogramBuckets+100)
	bounds := make([]float64, maxHistogramBuckets+50)
	for i := range buckets {
		buckets[i] = uint64(i)
	}
	for i := range bounds {
		bounds[i] = float64(i)
	}
	rm := []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			histMetric("http.duration", "ms", 42, 100.0, buckets, bounds),
		}}},
	}}
	out := MapOTLP(rm, time.Now())
	if len(out) != 1 {
		t.Fatalf("points = %d, want 1", len(out))
	}
	if got := len(out[0].BucketCounts); got != maxHistogramBuckets {
		t.Fatalf("BucketCounts len = %d, want %d", got, maxHistogramBuckets)
	}
	if got := len(out[0].ExplicitBounds); got != maxHistogramBuckets {
		t.Fatalf("ExplicitBounds len = %d, want %d", got, maxHistogramBuckets)
	}
}

// TestMapOTLPSingleMetricCapsDatapoints — одна метрика с > maxOTLPMetricPoints
// датапойнтов: кап проверяется ВНУТРИ цикла по точкам, поэтому результат ровно
// maxOTLPMetricPoints, а не аллоцирует весь гигантский массив (амплификация).
func TestMapOTLPSingleMetricCapsDatapoints(t *testing.T) {
	now := time.Now().UTC()
	dps := make([]*metricspb.NumberDataPoint, 0, maxOTLPMetricPoints+500)
	for i := 0; i < maxOTLPMetricPoints+500; i++ {
		dps = append(dps, &metricspb.NumberDataPoint{
			TimeUnixNano: uint64(now.Add(-time.Duration(i) * time.Second).UnixNano()),
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: float64(i)},
		})
	}
	rm := []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: "g", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: dps}},
		}}}},
	}}
	out := MapOTLP(rm, now)
	if len(out) != maxOTLPMetricPoints {
		t.Fatalf("single-metric datapoint cap len = %d, want %d", len(out), maxOTLPMetricPoints)
	}
}

// TestMapOTLPCapsStringLengths — name/unit/service/environment и ключи/значения
// атрибутов каппятся по длине (недоверенный ввод не должен раздувать колонки
// metric_points).
func TestMapOTLPCapsStringLengths(t *testing.T) {
	long := strings.Repeat("x", 500)
	longKey := strings.Repeat("k", 500)
	m := gaugeMetric(long, long, 1, kv(longKey, long))
	rm := []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			kv(attrServiceName, long),
			kv(attrDeployEnvName, long),
		}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{m}}},
	}}
	out := MapOTLP(rm, time.Now())
	if len(out) != 1 {
		t.Fatalf("points = %d, want 1", len(out))
	}
	p := out[0]
	if got := len([]rune(p.Name)); got != 200 {
		t.Fatalf("Name runes = %d, want 200", got)
	}
	if got := len([]rune(p.Unit)); got != 200 {
		t.Fatalf("Unit runes = %d, want 200", got)
	}
	if got := len([]rune(p.Service)); got != 200 {
		t.Fatalf("Service runes = %d, want 200", got)
	}
	if got := len([]rune(p.Environment)); got != 200 {
		t.Fatalf("Environment runes = %d, want 200", got)
	}
	// Ключ каппится до 64 рун, значение до 200.
	var gotKey, gotVal string
	for k, v := range p.Attributes {
		gotKey, gotVal = k, v
	}
	if got := len([]rune(gotKey)); got != 64 {
		t.Fatalf("attr key runes = %d, want 64", got)
	}
	if got := len([]rune(gotVal)); got != 200 {
		t.Fatalf("attr value runes = %d, want 200", got)
	}
}
