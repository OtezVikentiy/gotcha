package agent

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

// fullSample — полный тик (CPU != nil, все секции заполнены), опорный для
// большинства тестов эмиссии.
func fullSample() Sample {
	return Sample{
		Time:     time.Unix(2000, 0),
		CPU:      map[string]float64{"user": 0.3, "idle": 0.7},
		CPUCount: 4,
		Memory:   map[string]float64{"used": 0.4, "free": 0.6},
		Filesystems: []FSSample{
			{Device: "/dev/sda1", Mountpoint: "/", FSType: "ext4", Mode: "rw", Utilization: 0.5},
		},
		DiskIO: map[string]IOBytes{"sda": {Read: 100, Write: 200}},
		NetIO:  map[string]NetBytes{"eth0": {Recv: 10, Sent: 20}},
		Load1:  0.5, Load5: 0.4, Load15: 0.3,
		Procs:     map[string]int{"running": 2, "sleeping": 90},
		UptimeSec: 3600,
		BootTime:  time.Unix(1000, 0),
	}
}

// metricByName — хелпер поиска метрики в собранном экспорте по имени; nil,
// если метрики нет (первым делом проверяют именно отсутствие/наличие).
func metricByName(req *metricspb.MetricsData, name string) *metricspb.Metric {
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() == name {
					return m
				}
			}
		}
	}
	return nil
}

// attrValue — строковое значение атрибута датапойнта по ключу, "" если нет.
func attrValue(attrs []*commonpb.KeyValue, key string) string {
	for _, a := range attrs {
		if a.GetKey() == key {
			return a.GetValue().GetStringValue()
		}
	}
	return ""
}

func allMetricNames(req *metricspb.MetricsData) []string {
	var names []string
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				names = append(names, m.GetName())
			}
		}
	}
	return names
}

// TestBuildExportParity — контрактный тест паритета из спеки §5: набор имён
// метрик полного Sample должен совпадать с hostmetric.AllMetrics() один в
// один. Добавили имя в hostmetric — тест падает, пока агент его не эмитит.
func TestBuildExportParity(t *testing.T) {
	req := BuildExport("host1", "", "", fullSample())
	got := allMetricNames(req)
	want := hostmetric.AllMetrics()
	if len(got) != len(want) {
		t.Fatalf("метрик = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for _, name := range want {
		if metricByName(req, name) == nil {
			t.Errorf("метрика %q отсутствует в экспорте", name)
		}
	}
}

// TestBuildExportFirstTick — на первом тике (Sample.CPU == nil, нет дельты)
// CPUUtilization не эмитится, остальные метрики — как обычно.
func TestBuildExportFirstTick(t *testing.T) {
	s := fullSample()
	s.CPU = nil
	req := BuildExport("host1", "", "", s)
	if m := metricByName(req, hostmetric.CPUUtilization); m != nil {
		t.Errorf("%s присутствует на первом тике без CPU-дельты", hostmetric.CPUUtilization)
	}
	for _, name := range hostmetric.AllMetrics() {
		if name == hostmetric.CPUUtilization {
			continue
		}
		if metricByName(req, name) == nil {
			t.Errorf("метрика %q отсутствует на первом тике", name)
		}
	}
}

// TestBuildExportResource — resource-атрибуты: host.name из параметра,
// os.type фиксирован "linux", версия агента из version.Version().
func TestBuildExportResource(t *testing.T) {
	req := BuildExport("myhost.local", "", "", fullSample())
	if len(req.GetResourceMetrics()) != 1 {
		t.Fatalf("ResourceMetrics = %d, want 1", len(req.GetResourceMetrics()))
	}
	rm := req.GetResourceMetrics()[0]
	attrs := rm.GetResource().GetAttributes()
	if got := attrValue(attrs, "host.name"); got != "myhost.local" {
		t.Errorf("host.name = %q, want %q", got, "myhost.local")
	}
	if got := attrValue(attrs, "os.type"); got != "linux" {
		t.Errorf("os.type = %q, want %q", got, "linux")
	}
	if got := attrValue(attrs, hostmetric.AgentVersionAttr); got != version.Version() {
		t.Errorf("%s = %q, want %q", hostmetric.AgentVersionAttr, got, version.Version())
	}
	if len(rm.GetScopeMetrics()) != 1 {
		t.Fatalf("ScopeMetrics = %d, want 1", len(rm.GetScopeMetrics()))
	}
	if got := rm.GetScopeMetrics()[0].GetScope().GetName(); got != "gotcha-agent" {
		t.Errorf("scope name = %q, want %q", got, "gotcha-agent")
	}
}

// TestBuildExportLabels — deployment.environment/host.role в resource только
// при непустых значениях (спека §1.4: агент не эмитит пустые атрибуты).
func TestBuildExportLabels(t *testing.T) {
	req := BuildExport("h1", "prod", "web", Sample{Time: time.Unix(1, 0), BootTime: time.Unix(0, 0)})
	attrs := req.GetResourceMetrics()[0].GetResource().GetAttributes()
	got := map[string]string{}
	for _, kv := range attrs {
		got[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	if got["deployment.environment"] != "prod" || got["host.role"] != "web" {
		t.Fatalf("resource labels=%v", got)
	}
	// Пустые метки не эмитятся.
	req2 := BuildExport("h1", "", "", Sample{Time: time.Unix(1, 0), BootTime: time.Unix(0, 0)})
	for _, kv := range req2.GetResourceMetrics()[0].GetResource().GetAttributes() {
		if kv.GetKey() == "deployment.environment" || kv.GetKey() == "host.role" {
			t.Fatalf("пустая метка %q попала в resource", kv.GetKey())
		}
	}
}

// TestBuildExportCumulative — DiskIO/NetworkIO: Sum монотонный, кумулятивная
// темпоральность, StartTimeUnixNano == BootTime, атрибуты direction+device.
func TestBuildExportCumulative(t *testing.T) {
	s := fullSample()
	req := BuildExport("host1", "", "", s)
	wantStart := uint64(s.BootTime.UnixNano())

	diskM := metricByName(req, hostmetric.DiskIO)
	if diskM == nil {
		t.Fatal("system.disk.io отсутствует")
	}
	sum := diskM.GetSum()
	if sum == nil {
		t.Fatal("system.disk.io не Sum")
	}
	if !sum.GetIsMonotonic() {
		t.Error("system.disk.io должен быть монотонным")
	}
	if sum.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Errorf("temporality = %v, want CUMULATIVE", sum.GetAggregationTemporality())
	}
	if len(sum.GetDataPoints()) != 2 {
		t.Fatalf("disk io datapoints = %d, want 2 (read+write)", len(sum.GetDataPoints()))
	}
	seenDir := map[string]int64{}
	for _, dp := range sum.GetDataPoints() {
		if dp.GetStartTimeUnixNano() != wantStart {
			t.Errorf("disk io StartTimeUnixNano = %d, want %d", dp.GetStartTimeUnixNano(), wantStart)
		}
		if attrValue(dp.GetAttributes(), hostmetric.AttrDevice) != "sda" {
			t.Errorf("disk io device = %q, want sda", attrValue(dp.GetAttributes(), hostmetric.AttrDevice))
		}
		dir := attrValue(dp.GetAttributes(), hostmetric.AttrDirection)
		seenDir[dir] = dp.GetAsInt()
	}
	if seenDir["read"] != 100 || seenDir["write"] != 200 {
		t.Errorf("disk io direction values = %v, want read=100 write=200", seenDir)
	}

	netM := metricByName(req, hostmetric.NetworkIO)
	if netM == nil {
		t.Fatal("system.network.io отсутствует")
	}
	netSum := netM.GetSum()
	if netSum == nil || !netSum.GetIsMonotonic() {
		t.Fatal("system.network.io должен быть монотонным Sum")
	}
	if netSum.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Errorf("net temporality = %v, want CUMULATIVE", netSum.GetAggregationTemporality())
	}
	seenNetDir := map[string]int64{}
	for _, dp := range netSum.GetDataPoints() {
		if dp.GetStartTimeUnixNano() != wantStart {
			t.Errorf("net io StartTimeUnixNano = %d, want %d", dp.GetStartTimeUnixNano(), wantStart)
		}
		if attrValue(dp.GetAttributes(), hostmetric.AttrDevice) != "eth0" {
			t.Errorf("net io device = %q, want eth0", attrValue(dp.GetAttributes(), hostmetric.AttrDevice))
		}
		dir := attrValue(dp.GetAttributes(), hostmetric.AttrDirection)
		seenNetDir[dir] = dp.GetAsInt()
	}
	if seenNetDir["receive"] != 10 || seenNetDir["transmit"] != 20 {
		t.Errorf("net io direction values = %v, want receive=10 transmit=20", seenNetDir)
	}
}

// TestBuildExportGaugeAttrs — CPUUtilization: gauge с атрибутом state на
// каждом датапойнте; FilesystemUtilization: атрибуты device/mountpoint/
// type/mode; ProcessesCount: Sum non-monotonic по status.
func TestBuildExportGaugeAttrs(t *testing.T) {
	s := fullSample()
	req := BuildExport("host1", "", "", s)

	cpuM := metricByName(req, hostmetric.CPUUtilization)
	if cpuM == nil {
		t.Fatal("system.cpu.utilization отсутствует")
	}
	gauge := cpuM.GetGauge()
	if gauge == nil {
		t.Fatal("system.cpu.utilization должен быть Gauge")
	}
	if len(gauge.GetDataPoints()) != len(s.CPU) {
		t.Fatalf("cpu datapoints = %d, want %d", len(gauge.GetDataPoints()), len(s.CPU))
	}
	seenStates := map[string]float64{}
	for _, dp := range gauge.GetDataPoints() {
		state := attrValue(dp.GetAttributes(), hostmetric.AttrState)
		if state == "" {
			t.Error("cpu datapoint без атрибута state")
		}
		seenStates[state] = dp.GetAsDouble()
		if dp.GetTimeUnixNano() != uint64(s.Time.UnixNano()) {
			t.Errorf("cpu datapoint TimeUnixNano = %d, want %d", dp.GetTimeUnixNano(), s.Time.UnixNano())
		}
	}
	if seenStates["user"] != 0.3 || seenStates["idle"] != 0.7 {
		t.Errorf("cpu states = %v, want user=0.3 idle=0.7", seenStates)
	}

	fsM := metricByName(req, hostmetric.FilesystemUtilization)
	if fsM == nil {
		t.Fatal("system.filesystem.utilization отсутствует")
	}
	fsGauge := fsM.GetGauge()
	if fsGauge == nil || len(fsGauge.GetDataPoints()) != 1 {
		t.Fatalf("filesystem gauge datapoints = %v, want 1", fsGauge)
	}
	fsDP := fsGauge.GetDataPoints()[0]
	if attrValue(fsDP.GetAttributes(), hostmetric.AttrDevice) != "/dev/sda1" {
		t.Errorf("fs device = %q", attrValue(fsDP.GetAttributes(), hostmetric.AttrDevice))
	}
	if attrValue(fsDP.GetAttributes(), hostmetric.AttrMountpoint) != "/" {
		t.Errorf("fs mountpoint = %q", attrValue(fsDP.GetAttributes(), hostmetric.AttrMountpoint))
	}
	if attrValue(fsDP.GetAttributes(), hostmetric.AttrFSType) != "ext4" {
		t.Errorf("fs type = %q", attrValue(fsDP.GetAttributes(), hostmetric.AttrFSType))
	}
	if attrValue(fsDP.GetAttributes(), hostmetric.AttrFSMode) != "rw" {
		t.Errorf("fs mode = %q", attrValue(fsDP.GetAttributes(), hostmetric.AttrFSMode))
	}
	if fsDP.GetAsDouble() != 0.5 {
		t.Errorf("fs utilization = %v, want 0.5", fsDP.GetAsDouble())
	}

	procM := metricByName(req, hostmetric.ProcessesCount)
	if procM == nil {
		t.Fatal("system.processes.count отсутствует")
	}
	procSum := procM.GetSum()
	if procSum == nil {
		t.Fatal("system.processes.count должен быть Sum")
	}
	if procSum.GetIsMonotonic() {
		t.Error("system.processes.count не должен быть монотонным")
	}
	if len(procSum.GetDataPoints()) != len(s.Procs) {
		t.Fatalf("processes datapoints = %d, want %d", len(procSum.GetDataPoints()), len(s.Procs))
	}
	seenStatus := map[string]int64{}
	for _, dp := range procSum.GetDataPoints() {
		status := attrValue(dp.GetAttributes(), hostmetric.AttrStatus)
		seenStatus[status] = dp.GetAsInt()
	}
	if seenStatus["running"] != 2 || seenStatus["sleeping"] != 90 {
		t.Errorf("processes status = %v, want running=2 sleeping=90", seenStatus)
	}
}

// TestEncodeBodyGzipRoundTrip — EncodeBody: proto.Marshal + gzip; получатель
// (otlp.go) распаковывает Content-Encoding: gzip и должен получить исходный
// req обратно бит в бит.
func TestEncodeBodyGzipRoundTrip(t *testing.T) {
	req := BuildExport("host1", "", "", fullSample())
	body, err := EncodeBody(req)
	if err != nil {
		t.Fatalf("EncodeBody: %v", err)
	}

	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}

	var got metricspb.MetricsData
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	if !proto.Equal(&got, req) {
		t.Errorf("round-trip разошёлся: got %v, want %v", &got, req)
	}
}
