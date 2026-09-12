package metric

import "time"

// ПОЛНЫЙ список того, что может оказаться в MetricPoint.Type: MapOTLP тихо пропускает
// ExponentialHistogram/Summary как вне объёма. Закрытое множество.
const (
	TypeGauge     = "gauge"
	TypeSum       = "sum"
	TypeHistogram = "histogram"
)

// Источник истины для сторожа динамических ключей i18n ("metrics.type.").
var MetricTypes = []string{TypeGauge, TypeSum, TypeHistogram}

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
