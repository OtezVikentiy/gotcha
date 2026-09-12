package metric

import (
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Остальные ресурсные атрибуты едут в MetricPoint.Attributes как есть.
const (
	attrServiceName   = "service.name"
	attrDeployEnv     = "deployment.environment"      // старая семконвенция
	attrDeployEnvName = "deployment.environment.name" // текущая
	attrHostName      = "host.name"
)

// Защита от неограниченной кардинальности лейблов: берём первые maxAttrKeys
// в отсортированном порядке — детерминированно.
const maxAttrKeys = 64

// Потолок точек на один /v1/metrics-запрос — защита от раздувания памяти/CPU
// недоверенным экспортом с миллионами точек; лишние отбрасываются.
const maxOTLPMetricPoints = 10000

// Потолок длины массивов гистограммы: оба режутся СОГЛАСОВАННО (capHistogram),
// иначе ломается OTLP-инвариант len(bucket_counts)=len(explicit_bounds)+1.
const maxHistogramBuckets = 512

// NaN/Inf-значения отбрасываются.
func MapOTLP(resourceMetrics []*metricspb.ResourceMetrics, fallbackTS time.Time) []MetricPoint {
	var out []MetricPoint
	for _, rm := range resourceMetrics {
		service, environment, host := promote(rm.GetResource())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				out = mapMetric(out, m, service, environment, host, fallbackTS)
				if len(out) >= maxOTLPMetricPoints {
					return out
				}
			}
		}
	}
	return out
}

func mapMetric(out []MetricPoint, m *metricspb.Metric, service, environment, host string, fallbackTS time.Time) []MetricPoint {
	// Недоверенный ввод — каппим по длине, чтобы одна метрика не раздувала
	// CH-колонки metric_points.
	name, unit := capRunes(m.GetName(), 200), capRunes(m.GetUnit(), 200)
	base := func(ts uint64, attrs []*commonpb.KeyValue, typ string) (MetricPoint, bool) {
		t, ahead, ok := pointTime(ts, fallbackTS)
		if !ok {
			return MetricPoint{}, false
		}
		if ahead > 0 {
			clockSkew.note(ahead, host, fallbackTS)
		}
		return MetricPoint{
			Name: name, Type: typ, Unit: unit, Service: service, Environment: environment, Host: host,
			Attributes: attrsToMap(attrs), TS: t,
		}, true
	}
	switch data := m.GetData().(type) {
	case *metricspb.Metric_Gauge:
		for _, dp := range data.Gauge.GetDataPoints() {
			// Кап проверяем внутри цикла по датапойнтам: иначе одна метрика с гигантским
			// массивом проскочила бы внешнюю проверку, которая срабатывает лишь между метриками.
			if len(out) >= maxOTLPMetricPoints {
				return out
			}
			v, ok := numberValue(dp)
			if !ok {
				continue
			}
			p, ok := base(dp.GetTimeUnixNano(), dp.GetAttributes(), TypeGauge)
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
			if len(out) >= maxOTLPMetricPoints {
				return out
			}
			v, ok := numberValue(dp)
			if !ok {
				continue
			}
			p, ok := base(dp.GetTimeUnixNano(), dp.GetAttributes(), TypeSum)
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
			if len(out) >= maxOTLPMetricPoints {
				return out
			}
			sum := dp.GetSum()
			if math.IsNaN(sum) || math.IsInf(sum, 0) {
				continue
			}
			p, ok := base(dp.GetTimeUnixNano(), dp.GetAttributes(), TypeHistogram)
			if !ok {
				continue
			}
			p.Value = sum
			p.Count = dp.GetCount()
			p.BucketCounts, p.ExplicitBounds = capHistogram(dp.GetBucketCounts(), dp.GetExplicitBounds())
			p.Temporality = temp
			out = append(out, p)
		}
	default:
		// ExponentialHistogram/Summary — вне объёма, тихо пропускаются.
	}
	return out
}

// Вырезает NUL: PostgreSQL text отклоняет 0x00 (invalid byte sequence for
// encoding UTF8), а битое имя обрушило бы весь batch upsert хостов.
func capRunes(s string, n int) string {
	// IndexByte дёшев; ReplaceAll платится только строками с NUL.
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	// len(s)<=n в байтах гарантирует <=n рун (UTF-8 байт>=руна) — проверка до
	// []rune(s) экономит аллокацию на подавляющем большинстве коротких строк.
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Хвост счётчиков сверх потолка складывается в последний бакет (сумма
// сохраняется); при обрезке counts копируются, чтобы не портить входной протобуф.
func capHistogram(counts []uint64, bounds []float64) ([]uint64, []float64) {
	if len(bounds) > maxHistogramBuckets {
		bounds = bounds[:maxHistogramBuckets]
	}
	if len(counts) > maxHistogramBuckets+1 {
		capped := make([]uint64, maxHistogramBuckets+1)
		copy(capped, counts[:maxHistogramBuckets])
		var tail uint64
		for _, c := range counts[maxHistogramBuckets:] {
			tail += c
		}
		capped[maxHistogramBuckets] = tail
		counts = capped
	}
	return counts, bounds
}

// Порог нормалей IEEE-754 double (2⁻¹⁰²²): ниже него значение — субнормаль,
// бессмысленная как измерение и способная зациклить расчёт шкалы графика на отрисовке.
const minNormalFloat64 = 2.2250738585072014e-308

// Ни одна реальная метрика такой величины не достигает, а запас до math.MaxFloat64
// (~1.8e308) не даёт переполниться расчёту шкалы графика (targetLines=3) на входах ниже.
const maxSaneMagnitude = 1e300

// NaN/Inf/заведомо нефизичная величина → ok=false; субнормаль (включая денормализованный
// ноль) нормализуется в 0.
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
	if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) >= maxSaneMagnitude {
		return 0, false
	}
	if v != 0 && math.Abs(v) < minNormalFloat64 {
		v = 0
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

func promote(res *resourcepb.Resource) (service, environment, host string) {
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
		case attrHostName:
			host = attrString(kv.GetValue())
		}
	}
	// Каппим по длине: недоверенный ресурсный атрибут не должен раздувать
	// колонки metric_points.
	return capRunes(service, 200), capRunes(environment, 200), capRunes(host, 200)
}

func attrsToMap(attrs []*commonpb.KeyValue) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		if kv.GetKey() == "" {
			continue
		}
		// Недоверенные лейблы не должны раздувать колонку Attributes.
		m[capRunes(kv.GetKey(), 64)] = capRunes(attrString(kv.GetValue()), 200)
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

// Окно [now-90d, now] защищает партиции metric_points (toYYYYMM(ts)) от
// флуда точками за десятки месяцев; будущее клэмпится в pointTime, без допуска.
const maxPointAge = 90 * 24 * time.Hour

// Опережение в пределах порога — обычный джиттер сети/таймера, его не логируем,
// хотя клэмпим и считаем; лог — не чаще раза в интервал на процесс.
const (
	clockSkewLogThreshold = time.Minute
	clockSkewLogInterval  = time.Minute
)

// Без меток: имя хоста — высокая кардинальность, его место в логе, а не в
// self-метрике.
type clockSkewStats struct {
	total    atomic.Uint64 // всего клэмпнутых точек с начала процесса (self-метрика)
	pending  atomic.Uint64 // клэмпнуто с момента последней записи в лог
	maxAhead atomic.Int64  // максимальное опережение (нс) с момента последней записи в лог
	lastLog  atomic.Int64  // unix-наносекунды последней записи в лог; 0 — ещё не писали
}

var clockSkew clockSkewStats

// Лог — не чаще clockSkewLogInterval и только при опережении не меньше
// clockSkewLogThreshold; право записи берётся CAS по lastLog, гонки не дублируют.
func (s *clockSkewStats) note(ahead time.Duration, host string, now time.Time) {
	s.total.Add(1)
	s.pending.Add(1)
	for {
		cur := s.maxAhead.Load()
		if int64(ahead) <= cur || s.maxAhead.CompareAndSwap(cur, int64(ahead)) {
			break
		}
	}
	if ahead < clockSkewLogThreshold {
		return
	}
	last := s.lastLog.Load()
	if last != 0 && now.UnixNano()-last < int64(clockSkewLogInterval) {
		return
	}
	if !s.lastLog.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	slog.Warn("metric ingest: points with a timestamp from the future were clamped to the receive time",
		"points", s.pending.Swap(0), "max_ahead", time.Duration(s.maxAhead.Swap(0)), "host", host)
}

func ClockSkewPoints() int64 {
	return int64(clockSkew.total.Load())
}

// Будущее клэмпится к fallback, а не дропается или принимается как есть:
// иначе спешащие часы хоста делали бы его невидимым для фильтров ts<now.
func pointTime(ns uint64, fallback time.Time) (ts time.Time, ahead time.Duration, ok bool) {
	if ns == 0 {
		return fallback, 0, true
	}
	if ns > math.MaxInt64 {
		return time.Time{}, 0, false
	}
	ts = time.Unix(0, int64(ns)).UTC()
	if ts.Before(fallback.Add(-maxPointAge)) {
		return time.Time{}, 0, false
	}
	if ts.After(fallback) {
		return fallback, ts.Sub(fallback), true
	}
	return ts, 0, true
}
