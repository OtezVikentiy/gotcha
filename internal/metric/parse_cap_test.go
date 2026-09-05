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
// границ (защита от амплификации памяти/записи недоверенным экспортом), но
// СОГЛАСОВАННО: OTLP-инвариант len(counts) == len(bounds)+1 переживает
// обрезку, а отрезанный хвост счётчиков складывается в последний
// (бесконечный) бакет — сумма наблюдений сохраняется. Раньше оба массива
// резались порознь по одному лимиту, и у обрезанной гистограммы пропадал
// бесконечный бакет, а histogramQuantile читал границы со сдвигом.
func TestMapOTLPCapsHistogramBuckets(t *testing.T) {
	const bucketsIn = 600 // бакетов на входе: 600 границ, 601 счётчик
	bounds := make([]float64, bucketsIn)
	buckets := make([]uint64, bucketsIn+1)
	var sum uint64
	for i := range bounds {
		bounds[i] = float64(i)
	}
	for i := range buckets {
		buckets[i] = uint64(i)
		sum += uint64(i)
	}
	rm := []*metricspb.ResourceMetrics{{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			histMetric("http.duration", "ms", sum, 100.0, buckets, bounds),
		}}},
	}}
	out := MapOTLP(rm, time.Now())
	if len(out) != 1 {
		t.Fatalf("points = %d, want 1", len(out))
	}
	if got := len(out[0].ExplicitBounds); got != maxHistogramBuckets {
		t.Fatalf("ExplicitBounds len = %d, want %d", got, maxHistogramBuckets)
	}
	if got := len(out[0].BucketCounts); got != len(out[0].ExplicitBounds)+1 {
		t.Fatalf("BucketCounts len = %d, want len(bounds)+1 = %d", got, len(out[0].ExplicitBounds)+1)
	}
	var gotSum uint64
	for _, c := range out[0].BucketCounts {
		gotSum += c
	}
	if gotSum != sum {
		t.Fatalf("sum of capped BucketCounts = %d, want %d (tail folded into the last bucket)", gotSum, sum)
	}
	var tail uint64
	for _, c := range buckets[maxHistogramBuckets:] {
		tail += c
	}
	if got := out[0].BucketCounts[maxHistogramBuckets]; got != tail {
		t.Fatalf("last bucket = %d, want folded tail %d", got, tail)
	}
	if got := out[0].BucketCounts[maxHistogramBuckets-1]; got != buckets[maxHistogramBuckets-1] {
		t.Fatalf("bucket before the tail = %d, want untouched %d", got, buckets[maxHistogramBuckets-1])
	}
	if buckets[maxHistogramBuckets] != maxHistogramBuckets {
		t.Fatalf("input BucketCounts mutated: [%d] = %d", maxHistogramBuckets, buckets[maxHistogramBuckets])
	}
}

// TestCapHistogramLeavesShortArraysAlone — гистограмма в пределах потолка
// возвращается как есть, без копии и без изменений.
func TestCapHistogramLeavesShortArraysAlone(t *testing.T) {
	counts := []uint64{1, 2, 3}
	bounds := []float64{10, 20}
	gotCounts, gotBounds := capHistogram(counts, bounds)
	if &gotCounts[0] != &counts[0] || &gotBounds[0] != &bounds[0] {
		t.Fatalf("short arrays must be returned as-is, got copies")
	}
	if len(gotCounts) != 3 || len(gotBounds) != 2 {
		t.Fatalf("lens = %d/%d, want 3/2", len(gotCounts), len(gotBounds))
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
