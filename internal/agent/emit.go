// emit.go — сборка Sample (T4) в OTLP ExportMetricsServiceRequest и кодирование
// тела запроса. Правило спеки: агент неотличим по форме данных от коллектора
// OTel hostmetrics — набор метрик и атрибутов зеркалит internal/hostmetric
// (единый источник правды об именах для агента, web и генератора YAML
// коллектора).
package agent

import (
	"bytes"
	"compress/gzip"
	"maps"
	"slices"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

// scopeName — имя ScopeMetrics.Scope агента: тот же набор метрик, что у
// коллектора hostmetrics, но другой источник (различают по scope, не по
// имени метрики).
const scopeName = "gotcha-agent"

// direction-значения атрибута hostmetric.AttrDirection — semconv hostmetrics:
// диск read|write, сеть receive|transmit (разные глаголы для разных доменов).
const (
	directionRead     = "read"
	directionWrite    = "write"
	directionReceive  = "receive"
	directionTransmit = "transmit"
)

// BuildExport собирает один тик Sample в OTLP-экспорт: один ResourceMetrics +
// один ScopeMetrics. На первом тике (s.CPU == nil — ещё нет дельты, см.
// Collector.cpuUtilization) system.cpu.utilization не эмитится, остальные
// метрики идут как обычно.
//
// Возвращает metricspb.MetricsData, а не collector-обёртку
// ExportMetricsServiceRequest: сервер (internal/ingest/otlp.go,
// otlpUnmarshalMetrics) анмаршалит тело POST /v1/metrics именно в
// MetricsData. У обоих типов ровно одно поле — repeated ResourceMetrics — и
// wire-формат идентичен, но пакет collector/metrics/v1 несёт в той же
// директории сгенерённый gRPC-gateway код и тащит grpc+grpc-gateway в
// зависимости и в бинарь агента, который сам никогда не ходит по gRPC.
func BuildExport(hostname string, s Sample) *metricspb.MetricsData {
	ts := uint64(s.Time.UnixNano())
	bootNano := uint64(s.BootTime.UnixNano())

	var metrics []*metricspb.Metric
	if s.CPU != nil {
		metrics = append(metrics, gaugeMetric(hostmetric.CPUUtilization, stateDataPoints(s.CPU, ts, hostmetric.AttrState)))
	}
	metrics = append(metrics,
		sumMetric(hostmetric.CPULogicalCount, false, []*metricspb.NumberDataPoint{
			intDataPoint(ts, 0, int64(s.CPUCount), nil),
		}),
		gaugeMetric(hostmetric.MemoryUtilization, stateDataPoints(s.Memory, ts, hostmetric.AttrState)),
		gaugeMetric(hostmetric.FilesystemUtilization, filesystemDataPoints(s.Filesystems, ts)),
		sumMetric(hostmetric.DiskIO, true, diskIODataPoints(s.DiskIO, ts, bootNano)),
		sumMetric(hostmetric.NetworkIO, true, netIODataPoints(s.NetIO, ts, bootNano)),
		gaugeMetric(hostmetric.LoadAvg1m, []*metricspb.NumberDataPoint{doubleDataPoint(ts, s.Load1, nil)}),
		gaugeMetric(hostmetric.LoadAvg5m, []*metricspb.NumberDataPoint{doubleDataPoint(ts, s.Load5, nil)}),
		gaugeMetric(hostmetric.LoadAvg15m, []*metricspb.NumberDataPoint{doubleDataPoint(ts, s.Load15, nil)}),
		sumMetric(hostmetric.ProcessesCount, false, statusDataPoints(s.Procs, ts)),
		gaugeMetric(hostmetric.Uptime, []*metricspb.NumberDataPoint{doubleDataPoint(ts, s.UptimeSec, nil)}),
	)

	return &metricspb.MetricsData{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				stringAttr("host.name", hostname),
				stringAttr("os.type", "linux"),
				stringAttr(hostmetric.AgentVersionAttr, version.Version()),
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Scope:   &commonpb.InstrumentationScope{Name: scopeName},
				Metrics: metrics,
			}},
		}},
	}
}

// EncodeBody сериализует запрос в protobuf и сжимает gzip — приёмник
// (internal/ingest/otlp.go) распаковывает тело по Content-Encoding: gzip тем
// же путём, что и у остальных OTLP-источников.
func EncodeBody(req *metricspb.MetricsData) ([]byte, error) {
	raw, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// stateDataPoints — gauge-точки по карте state→доля (CPU/Memory): один
// датапойнт на state, ключи отсортированы для стабильного порядка.
func stateDataPoints(m map[string]float64, ts uint64, attrKey string) []*metricspb.NumberDataPoint {
	dps := make([]*metricspb.NumberDataPoint, 0, len(m))
	for _, state := range slices.Sorted(maps.Keys(m)) {
		dps = append(dps, doubleDataPoint(ts, m[state], []*commonpb.KeyValue{stringAttr(attrKey, state)}))
	}
	return dps
}

// statusDataPoints — Sum-точки system.processes.count по status→count.
func statusDataPoints(m map[string]int, ts uint64) []*metricspb.NumberDataPoint {
	dps := make([]*metricspb.NumberDataPoint, 0, len(m))
	for _, status := range slices.Sorted(maps.Keys(m)) {
		dps = append(dps, intDataPoint(ts, 0, int64(m[status]), []*commonpb.KeyValue{stringAttr(hostmetric.AttrStatus, status)}))
	}
	return dps
}

// filesystemDataPoints — gauge-точки system.filesystem.utilization, по одной
// на раздел (Collect уже отфильтровал псевдо-ФС и служебные точки
// монтирования — hostmetric.Excluded*); атрибуты берутся из FSSample целиком.
func filesystemDataPoints(fs []FSSample, ts uint64) []*metricspb.NumberDataPoint {
	dps := make([]*metricspb.NumberDataPoint, 0, len(fs))
	for _, f := range fs {
		attrs := []*commonpb.KeyValue{
			stringAttr(hostmetric.AttrDevice, f.Device),
			stringAttr(hostmetric.AttrMountpoint, f.Mountpoint),
			stringAttr(hostmetric.AttrFSType, f.FSType),
			stringAttr(hostmetric.AttrFSMode, f.Mode),
		}
		dps = append(dps, doubleDataPoint(ts, f.Utilization, attrs))
	}
	return dps
}

// diskIODataPoints — Sum-точки system.disk.io: по устройству две точки
// (read/write); StartTimeUnixNano = BootTime — счётчик since-boot, не дельта
// (см. IOBytes).
func diskIODataPoints(m map[string]IOBytes, ts, bootNano uint64) []*metricspb.NumberDataPoint {
	dps := make([]*metricspb.NumberDataPoint, 0, len(m)*2)
	for _, device := range slices.Sorted(maps.Keys(m)) {
		io := m[device]
		dps = append(dps,
			intDataPoint(ts, bootNano, int64(io.Read), []*commonpb.KeyValue{
				stringAttr(hostmetric.AttrDevice, device), stringAttr(hostmetric.AttrDirection, directionRead),
			}),
			intDataPoint(ts, bootNano, int64(io.Write), []*commonpb.KeyValue{
				stringAttr(hostmetric.AttrDevice, device), stringAttr(hostmetric.AttrDirection, directionWrite),
			}),
		)
	}
	return dps
}

// netIODataPoints — Sum-точки system.network.io: по интерфейсу две точки
// (receive/transmit); StartTimeUnixNano = BootTime (тот же счётчик since-boot).
func netIODataPoints(m map[string]NetBytes, ts, bootNano uint64) []*metricspb.NumberDataPoint {
	dps := make([]*metricspb.NumberDataPoint, 0, len(m)*2)
	for _, iface := range slices.Sorted(maps.Keys(m)) {
		n := m[iface]
		dps = append(dps,
			intDataPoint(ts, bootNano, int64(n.Recv), []*commonpb.KeyValue{
				stringAttr(hostmetric.AttrDevice, iface), stringAttr(hostmetric.AttrDirection, directionReceive),
			}),
			intDataPoint(ts, bootNano, int64(n.Sent), []*commonpb.KeyValue{
				stringAttr(hostmetric.AttrDevice, iface), stringAttr(hostmetric.AttrDirection, directionTransmit),
			}),
		)
	}
	return dps
}

// stringAttr — единственный вид значения атрибута, который эмитит агент:
// state/device/direction/mountpoint/type/mode/status — всё строки semconv
// hostmetrics.
func stringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func doubleDataPoint(ts uint64, v float64, attrs []*commonpb.KeyValue) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{
		TimeUnixNano: ts,
		Attributes:   attrs,
		Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: v},
	}
}

func intDataPoint(ts, startNano uint64, v int64, attrs []*commonpb.KeyValue) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{
		StartTimeUnixNano: startNano,
		TimeUnixNano:      ts,
		Attributes:        attrs,
		Value:             &metricspb.NumberDataPoint_AsInt{AsInt: v},
	}
}

func gaugeMetric(name string, dps []*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: dps}}}
}

// sumMetric — все Sum-метрики агента кумулятивные (AGGREGATION_TEMPORALITY_
// CUMULATIVE), монотонность зависит от смысла метрики (счётчики since-boot
// монотонны, снимки состояния вроде processes.count — нет).
func sumMetric(name string, monotonic bool, dps []*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
		DataPoints:             dps,
		AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
		IsMonotonic:            monotonic,
	}}}
}
