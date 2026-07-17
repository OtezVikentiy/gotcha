package metric

import (
	"math"
	"sort"
	"strconv"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Атрибуты ресурса, которые промотируем в поля модели (та же семантика, что у
// спанов этапа 3): всё остальное едет в MetricPoint.Attributes как есть.
const (
	attrServiceName   = "service.name"
	attrDeployEnv     = "deployment.environment"      // старая семконвенция
	attrDeployEnvName = "deployment.environment.name" // текущая
)

// maxAttrKeys — кап числа лейблов точки (тот же приём, что maxDataKeys у спанов):
// защита от неограниченной кардинальности. Берём первые maxAttrKeys в
// отсортированном порядке — детерминированно.
const maxAttrKeys = 64

// MapOTLP разворачивает OTLP ResourceMetrics в плоские MetricPoint'ы, готовые к
// записи. Поддерживаются Gauge, Sum, Histogram; ExponentialHistogram/Summary
// пропускаются (вне объёма этапа). NaN/Inf-значения отбрасываются. fallbackTS —
// метка времени для точек с нулевым TimeUnixNano.
func MapOTLP(resourceMetrics []*metricspb.ResourceMetrics, fallbackTS time.Time) []MetricPoint {
	var out []MetricPoint
	for _, rm := range resourceMetrics {
		service, environment := promote(rm.GetResource())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				out = mapMetric(out, m, service, environment, fallbackTS)
			}
		}
	}
	return out
}

func mapMetric(out []MetricPoint, m *metricspb.Metric, service, environment string, fallbackTS time.Time) []MetricPoint {
	name, unit := m.GetName(), m.GetUnit()
	base := func(ts uint64, attrs []*commonpb.KeyValue, typ string) (MetricPoint, bool) {
		t, ok := pointTime(ts, fallbackTS)
		if !ok {
			return MetricPoint{}, false
		}
		return MetricPoint{
			Name: name, Type: typ, Unit: unit, Service: service, Environment: environment,
			Attributes: attrsToMap(attrs), TS: t,
		}, true
	}
	switch data := m.GetData().(type) {
	case *metricspb.Metric_Gauge:
		for _, dp := range data.Gauge.GetDataPoints() {
			v, ok := numberValue(dp)
			if !ok {
				continue
			}
			p, ok := base(dp.GetTimeUnixNano(), dp.GetAttributes(), "gauge")
			if !ok {
				continue
			}
			p.Value = v
			out = append(out, p)
		}
	case *metricspb.Metric_Sum:
		mono := data.Sum.GetIsMonotonic()
		temp := temporalityString(data.Sum.GetAggregationTemporality())
		for _, dp := range data.Sum.GetDataPoints() {
			v, ok := numberValue(dp)
			if !ok {
				continue
			}
			p, ok := base(dp.GetTimeUnixNano(), dp.GetAttributes(), "sum")
			if !ok {
				continue
			}
			p.Value = v
			p.Monotonic = mono
			p.Temporality = temp
			out = append(out, p)
		}
	case *metricspb.Metric_Histogram:
		temp := temporalityString(data.Histogram.GetAggregationTemporality())
		for _, dp := range data.Histogram.GetDataPoints() {
			sum := dp.GetSum()
			if math.IsNaN(sum) || math.IsInf(sum, 0) {
				continue
			}
			p, ok := base(dp.GetTimeUnixNano(), dp.GetAttributes(), "histogram")
			if !ok {
				continue
			}
			p.Value = sum
			p.Count = dp.GetCount()
			p.BucketCounts = dp.GetBucketCounts()
			p.ExplicitBounds = dp.GetExplicitBounds()
			p.Temporality = temp
			out = append(out, p)
		}
	default:
		// ExponentialHistogram / Summary / прочее — вне объёма, тихо пропускаем.
	}
	return out
}

// numberValue достаёт значение скалярной точки (double или int); NaN/Inf → false.
func numberValue(dp *metricspb.NumberDataPoint) (float64, bool) {
	var v float64
	switch dp.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		v = dp.GetAsDouble()
	case *metricspb.NumberDataPoint_AsInt:
		v = float64(dp.GetAsInt())
	default:
		return 0, false
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func temporalityString(t metricspb.AggregationTemporality) string {
	switch t {
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE:
		return "cumulative"
	case metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA:
		return "delta"
	default:
		return ""
	}
}

// promote вытаскивает service.name и environment из ресурсных атрибутов.
func promote(res *resourcepb.Resource) (service, environment string) {
	for _, kv := range res.GetAttributes() {
		switch kv.GetKey() {
		case attrServiceName:
			service = attrString(kv.GetValue())
		case attrDeployEnvName:
			environment = attrString(kv.GetValue())
		case attrDeployEnv:
			if environment == "" {
				environment = attrString(kv.GetValue())
			}
		}
	}
	return service, environment
}

// attrsToMap собирает datapoint-атрибуты в строковый Map (кап maxAttrKeys,
// детерминированно по отсортированным ключам).
func attrsToMap(attrs []*commonpb.KeyValue) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		if kv.GetKey() == "" {
			continue
		}
		m[kv.GetKey()] = attrString(kv.GetValue())
	}
	if len(m) <= maxAttrKeys {
		return m
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	capped := make(map[string]string, maxAttrKeys)
	for _, k := range keys[:maxAttrKeys] {
		capped[k] = m[k]
	}
	return capped
}

// attrString читает скалярное представление AnyValue (для лейблов/ресурса).
func attrString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		if x.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	}
	return ""
}

// Окно допустимых таймстемпов метрик: [now-90d, now+1d]. Как у событий/трасс
// (см. ingest/timestamp.go), защищает партиции metric_points (PARTITION BY
// toYYYYMM(ts)) от флуда точками, разнесёнными по десяткам месяцев в одном батче.
const (
	maxPointAge    = 90 * 24 * time.Hour
	maxPointFuture = 24 * time.Hour
)

// pointTime переводит наносекунды OTLP в момент времени. Возвращает ok=false для
// мусора: ns > MaxInt64 (не влезает в int64) и времени вне окна ретенции — такие
// точки писатель пропускает. ns == 0 (поле не заполнено) → fallback, ok=true.
func pointTime(ns uint64, fallback time.Time) (time.Time, bool) {
	if ns == 0 {
		return fallback, true
	}
	if ns > math.MaxInt64 {
		return time.Time{}, false
	}
	ts := time.Unix(0, int64(ns)).UTC()
	if ts.Before(fallback.Add(-maxPointAge)) || ts.After(fallback.Add(maxPointFuture)) {
		return time.Time{}, false
	}
	return ts, true
}
