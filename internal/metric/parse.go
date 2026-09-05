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

// Атрибуты ресурса, которые промотируем в поля модели (та же семантика, что у
// спанов этапа 3): всё остальное едет в MetricPoint.Attributes как есть.
const (
	attrServiceName   = "service.name"
	attrDeployEnv     = "deployment.environment"      // старая семконвенция
	attrDeployEnvName = "deployment.environment.name" // текущая
	attrHostName      = "host.name"
)

// maxAttrKeys — кап числа лейблов точки (тот же приём, что maxDataKeys у спанов):
// защита от неограниченной кардинальности. Берём первые maxAttrKeys в
// отсортированном порядке — детерминированно.
const maxAttrKeys = 64

// MapOTLP разворачивает OTLP ResourceMetrics в плоские MetricPoint'ы, готовые к
// записи. Поддерживаются Gauge, Sum, Histogram; ExponentialHistogram/Summary
// пропускаются (вне объёма этапа). NaN/Inf-значения отбрасываются. fallbackTS —
// метка времени для точек с нулевым TimeUnixNano.
// maxOTLPMetricPoints — потолок числа датапойнтов, разбираемых из одного OTLP
// /v1/metrics-запроса. Без него недоверенный экспорт с миллионами точек раздул
// бы память/CPU в обход дисциплины maxEnvelopeItems Sentry-пути. Щедрый, но
// конечный; лишние точки отбрасываются.
const maxOTLPMetricPoints = 10000

// maxHistogramBuckets — потолок длины массивов гистограммы (bucket_counts и
// explicit_bounds) на одну точку. Реальные гистограммы столько границ не имеют;
// без капа недоверенный экспорт с гигантскими массивами раздул бы память и запись
// в metric_points (тот же приём защиты от амплификации, что maxOTLPMetricPoints).
// Оба массива обрезаются СОГЛАСОВАННО (capHistogram): OTLP-контракт —
// len(bucket_counts) = len(explicit_bounds)+1, и его читает histogramQuantile:
// границ не больше maxHistogramBuckets, счётчиков — на один больше.
const maxHistogramBuckets = 512

func MapOTLP(resourceMetrics []*metricspb.ResourceMetrics, fallbackTS time.Time) []MetricPoint {
	var out []MetricPoint
	for _, rm := range resourceMetrics {
		service, environment, host := promote(rm.GetResource())
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				out = mapMetric(out, m, service, environment, host, fallbackTS)
				// mapMetric сам не переваливает за maxOTLPMetricPoints (кап внутри
				// циклов по датапойнтам), поэтому достигнув потолка просто выходим.
				if len(out) >= maxOTLPMetricPoints {
					return out
				}
			}
		}
	}
	return out
}

func mapMetric(out []MetricPoint, m *metricspb.Metric, service, environment, host string, fallbackTS time.Time) []MetricPoint {
	// name/unit — недоверенный ввод, каппим по длине (как имя/юнит спанов в otlp.go),
	// чтобы одна метрика не раздувала CH-колонки metric_points.
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
			// Кап проверяем ВНУТРИ цикла по датапойнтам (как maxOTLPSpans в otlp.go):
			// одна метрика с гигантским массивом точек иначе аллоцировала бы всё,
			// проскочив внешнюю проверку в MapOTLP (она срабатывает лишь между метриками).
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
		// ExponentialHistogram / Summary / прочее — вне объёма, тихо пропускаем.
	}
	return out
}

// capRunes вырезает NUL и обрезает s до n рун. Локальный аналог одноимённого
// хелпера из internal/ingest — недоверенные OTLP-строки (name/unit/service/
// атрибуты) не должны раздувать CH-колонки без ограничения. Не тащим
// зависимость на ingest.
//
// NUL вырезается ровно по той же причине, что и в ingest.capRunes: protobuf и
// ClickHouse его принимают, а PostgreSQL на text падает («invalid byte sequence
// for encoding UTF8: 0x00»). Промотированный host.name из этих строк доезжает
// до PG (реестр hosts, батчевый unnest в host.Store.Upsert), и одно битое имя
// роняло бы upsert всего батча — вместе с обновлением last_seen соседних живых
// хостов, которым оценщик тут же открыл бы ложный silent-инцидент.
func capRunes(s string, n int) string {
	// IndexByte — дешёвый просмотр без аллокаций; ReplaceAll платится только
	// строками, реально несущими NUL.
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	// Быстрый путь без единой аллокации. В UTF-8 байт всегда не меньше, чем рун,
	// поэтому len(s) <= n гарантирует, что рун тоже не больше n. Проверка стоит
	// ДО []rune(s) намеренно: подавляющее большинство строк приёма короткие, и
	// раньше каждая из них платила копией всей строки в срез рун — на индексных
	// профилях это давало миллионы копий и гигабайты аллокаций.
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// capHistogram обрезает массивы гистограммы перед записью: границы — до
// maxHistogramBuckets, счётчики — до maxHistogramBuckets+1, чтобы OTLP-инвариант
// len(counts) == len(bounds)+1 пережил обрезку. Раньше оба массива резались
// порознь по одному лимиту, и у гистограммы длиннее потолка счётчиков
// становилось столько же, сколько границ: последний (бесконечный) бакет
// пропадал, а histogramQuantile, считающий бакеты по контракту, читал границы
// со сдвигом. Хвост счётчиков не выбрасывается, а складывается в последний
// оставшийся бакет: после обрезки он и есть (bounds[N-1], +inf), и всё, что
// лежало выше отрезанных границ, по определению попадает именно в него —
// сумма наблюдений сохраняется, квантили выше потолка честно упираются в
// последнюю границу. Срезы без обрезки разделяют память исходного массива;
// при обрезке счётчики копируются, чтобы не портить входной протобуф.
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

// promote вытаскивает service.name, environment и host.name из ресурсных атрибутов.
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
	// Каппим промотируемые поля по длине (как service/environment спанов в otlp.go):
	// недоверенный ресурсный атрибут не должен раздувать колонки metric_points.
	return capRunes(service, 200), capRunes(environment, 200), capRunes(host, 200)
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
		// Каппим ключ и значение по длине (как otlpAttrMap у спанов: 64 руны ключ,
		// 200 значение) — недоверенные лейблы не должны раздувать колонку Attributes.
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

// Окно допустимых таймстемпов метрик: [now-90d, now]. Как у событий/трасс
// (см. ingest/timestamp.go), защищает партиции metric_points (PARTITION BY
// toYYYYMM(ts)) от флуда точками, разнесёнными по десяткам месяцев в одном батче.
// Верхней границы с допуском нет: любое будущее приводится к моменту приёма
// (см. pointTime).
const maxPointAge = 90 * 24 * time.Hour

// clockSkewLogThreshold — опережение, начиная с которого клэмп попадает в лог.
// Сетевой джиттер и округление таймера экспортёра дают опережение в
// миллисекунды; клэмпить и считать их нужно, а писать в лог — нет.
// clockSkewLogInterval — не чаще одной записи в лог на процесс за этот период.
const (
	clockSkewLogThreshold = time.Minute
	clockSkewLogInterval  = time.Minute
)

// clockSkewStats — процесс-локальный учёт точек, пришедших со временем из
// будущего и приведённых к моменту приёма (K3-1). Без блокировок и без меток:
// имя хоста — высокая кардинальность, ему место в логе, а не в self-метрике.
type clockSkewStats struct {
	total    atomic.Uint64 // всего клэмпнутых точек с начала процесса (self-метрика)
	pending  atomic.Uint64 // клэмпнуто с момента последней записи в лог
	maxAhead atomic.Int64  // максимальное опережение (нс) с момента последней записи в лог
	lastLog  atomic.Int64  // unix-наносекунды последней записи в лог; 0 — ещё не писали
}

var clockSkew clockSkewStats

// note учитывает одну клэмпнутую точку с опережением ahead. В лог попадает
// только опережение не меньше clockSkewLogThreshold и не чаще
// clockSkewLogInterval на процесс: запись забирает накопленное «сколько и
// насколько» с прошлой записи, так что редкий лог остаётся полным отчётом, а
// не выборкой одной точки. Право на запись берётся CAS'ом по lastLog —
// параллельные приёмники не напишут её дважды.
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

// ClockSkewPoints — снимок счётчика точек, пришедших со временем из будущего
// и приведённых к моменту приёма, с начала процесса. Потокобезопасно и дёшево —
// self-метрика main читает его как func() int64 при каждом снятии показаний.
func ClockSkewPoints() int64 {
	return int64(clockSkew.total.Load())
}

// pointTime переводит наносекунды OTLP в момент времени. Возвращает ok=false для
// мусора: ns > MaxInt64 (не влезает в int64) и времени старше окна ретенции —
// такие точки писатель пропускает. ns == 0 (поле не заполнено) → fallback.
//
// Время из будущего не дропается и не принимается как есть, а КЛЭМПИТСЯ к
// fallback (момент приёма); ahead > 0 говорит, на сколько точка опережала.
// Раньше будущее до суток принималось как есть, а дальше — дропалось. Но все
// читатели режут окно по часам сервера (host/evaluator.go, metric/query.go —
// ts < now), и принятая «как есть» точка из будущего для них равна
// потерянной: хост со спешащими на минуты часами молча не существовал для
// порогового эвалюатора и графиков, пока его точки не «дозревали». Клэмп
// делает данные видимыми сразу, а расхождение часов — наблюдаемым: счётчик
// gotcha_metric_points_clock_skew_total и лог (clockSkewStats).
//
// Порядок точек одной серии при клэмпе сохраняется МЕЖДУ батчами (момент
// приёма растёт монотонно), но не ВНУТРИ батча: несколько опережающих точек
// одной серии в одном запросе ложатся на один и тот же fallback и становятся
// неразличимы по ts. Единственное практическое следствие — агрегат last
// (query.go, argMax(value, ts)) при такой ничьей выберет любую из них.
// Штатный Go-агент шлёт одну точку на серию в батче, так что случай — это
// сторонние экспортёры с буферизацией и спешащими часами одновременно.
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
