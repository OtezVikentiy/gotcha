// Package metric — приём, хранение, запрос и алертинг OTLP-метрик (этап 6).
package metric

import "time"

// Типы метрики, которые реально долетают до metric_points через MapOTLP
// (internal/metric/parse.go, mapMetric): OTLP знает больше форм
// (ExponentialHistogram, Summary), но mapMetric их тихо пропускает как «вне
// объёма этапа» — то есть TypeGauge/TypeSum/TypeHistogram не просто
// распространённые значения, а ПОЛНЫЙ список того, что может оказаться в
// MetricPoint.Type/MetricInfo.Type. Закрытое множество, в отличие от
// агрегации правил (metric.Aggregations) — то тоже закрытое, но по другой
// причине (перечисление switch в metricAggFor/metricAggOptions).
const (
	TypeGauge     = "gauge"
	TypeSum       = "sum"
	TypeHistogram = "histogram"
)

// MetricTypes — все типы метрики. Источник истины для сторожа динамических
// ключей (группа i18n "metrics.type.", internal/guards/i18n_dynamic_test.go).
var MetricTypes = []string{TypeGauge, TypeSum, TypeHistogram}

// MetricPoint — одна datapoint метрики, готовая к записи в metric_points.
type MetricPoint struct {
	Name, Type, Unit, Service, Environment string
	Host                                   string // промоутированный ресурсный host.name, пусто у метрик приложений
	Attributes                             map[string]string
	TS                                     time.Time
	Value                                  float64  // sum/gauge: значение; histogram: sum наблюдений
	Count                                  uint64   // histogram: число наблюдений
	BucketCounts                           []uint64 // histogram
	ExplicitBounds                         []float64
	Monotonic                              bool   // sum
	Temporality                            string // 'cumulative'|'delta'|''
}
