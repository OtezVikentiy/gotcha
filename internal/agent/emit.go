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

// Тот же набор метрик, что у коллектора hostmetrics — различаются по scope,
// не по имени метрики.
const scopeName = "gotcha-agent"

// Диск использует read|write, сеть — receive|transmit: разные глаголы для
// разных доменов (semconv hostmetrics).
const (
	directionRead     = "read"
	directionWrite    = "write"
	directionReceive  = "receive"
	directionTransmit = "transmit"
)

// Возвращает MetricsData, не ExportMetricsServiceRequest: сервер анмаршалит
// тело именно в MetricsData, а последняя тащит grpc+grpc-gateway в зависимости.
func BuildExport(hostname, environment, role string, s Sample) *metricspb.MetricsData {
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

	attrs := []*commonpb.KeyValue{
		stringAttr("host.name", hostname),
		stringAttr("os.type", "linux"),
		stringAttr(hostmetric.AgentVersionAttr, version.Version()),
	}
	if environment != "" {
		attrs = append(attrs, stringAttr("deployment.environment", environment))
	}
	if role != "" {
		attrs = append(attrs, stringAttr("host.role", role))
	}

	return &metricspb.MetricsData{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: attrs},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Scope:   &commonpb.InstrumentationScope{Name: scopeName},
				Metrics: metrics,
			}},
		}},
	}
}

// Приёмник (internal/ingest/otlp.go) распаковывает по Content-Encoding: gzip —
// тем же путём, что остальные OTLP-источники.
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

// Ключи отсортированы для стабильного порядка датапойнтов.
func stateDataPoints(m map[string]float64, ts uint64, attrKey string) []*metricspb.NumberDataPoint {
	dps := make([]*metricspb.NumberDataPoint, 0, len(m))
	for _, state := range slices.Sorted(maps.Keys(m)) {
		dps = append(dps, doubleDataPoint(ts, m[state], []*commonpb.KeyValue{stringAttr(attrKey, state)}))
	}
	return dps
}

func statusDataPoints(m map[string]int, ts uint64) []*metricspb.NumberDataPoint {
	dps := make([]*metricspb.NumberDataPoint, 0, len(m))
	for _, status := range slices.Sorted(maps.Keys(m)) {
		dps = append(dps, intDataPoint(ts, 0, int64(m[status]), []*commonpb.KeyValue{stringAttr(hostmetric.AttrStatus, status)}))
	}
	return dps
}

// Collect уже отфильтровал псевдо-ФС и служебные точки монтирования — тут
// фильтрации нет.
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

// StartTimeUnixNano = BootTime: счётчик since-boot, не дельта (см. IOBytes).
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

// StartTimeUnixNano = BootTime, тот же счётчик since-boot, что и diskIODataPoints.
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

// Все Sum-метрики кумулятивные; monotonic зависит от смысла метрики (счётчики
// since-boot — да, снимки состояния вроде processes.count — нет).
func sumMetric(name string, monotonic bool, dps []*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{Name: name, Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
		DataPoints:             dps,
		AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
		IsMonotonic:            monotonic,
	}}}
}
