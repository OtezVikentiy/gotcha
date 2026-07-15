// Package metric — приём, хранение, запрос и алертинг OTLP-метрик (этап 6).
package metric

import "time"

// MetricPoint — одна datapoint метрики, готовая к записи в metric_points.
type MetricPoint struct {
	Name, Type, Unit, Service, Environment string
	Attributes                             map[string]string
	TS                                     time.Time
	Value                                  float64  // sum/gauge: значение; histogram: sum наблюдений
	Count                                  uint64   // histogram: число наблюдений
	BucketCounts                           []uint64 // histogram
	ExplicitBounds                         []float64
	Monotonic                              bool   // sum
	Temporality                            string // 'cumulative'|'delta'|''
}
