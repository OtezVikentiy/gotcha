package ingest

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/agent"
	"gitflic.ru/otezvikentiy/gotcha/internal/hostmetric"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

func fakeAgentSample() agent.Sample {
	return agent.Sample{
		Time:     time.Now(),
		CPU:      map[string]float64{"user": 0.1, "idle": 0.9},
		CPUCount: 4,
		Memory:   map[string]float64{"used": 0.4, "free": 0.6},
		Filesystems: []agent.FSSample{
			{Device: "/dev/sda1", Mountpoint: "/", FSType: "ext4", Mode: "rw", Utilization: 0.5},
		},
		DiskIO:    map[string]agent.IOBytes{"sda": {Read: 100, Write: 200}},
		NetIO:     map[string]agent.NetBytes{"eth0": {Recv: 1, Sent: 2}},
		Load1:     0.5,
		Load5:     0.4,
		Load15:    0.3,
		Procs:     map[string]int{"running": 2, "sleeping": 90},
		UptimeSec: 3600,
		BootTime:  time.Now().Add(-time.Hour),
	}
}

func TestAgentExportEndToEnd(t *testing.T) {
	sink := &collectMetricSink{}
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindAgent}}), nil, nil, 1<<20)
	h.Metrics = sink

	md := agent.BuildExport("web-1", "", "", fakeAgentSample(), 0, uint64(time.Now().Add(-time.Hour).UnixNano()))
	body, err := agent.EncodeBody(md)
	if err != nil {
		t.Fatalf("EncodeBody: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/metrics", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer pub")
	w := httptest.NewRecorder()
	h.otlpMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "partial_success") {
		t.Errorf("тело 200-ответа содержит partial_success: %q", w.Body.String())
	}

	gotNames := map[string]bool{}
	var dropped *metric.MetricPoint
	for i, p := range sink.points {
		gotNames[p.Name] = true
		if p.Host != "web-1" {
			t.Errorf("точка %q: Host = %q, want web-1 (host.name должен быть промоутирован)", p.Name, p.Host)
		}
		if _, leaked := p.Attributes[hostmetric.AgentVersionAttr]; leaked {
			t.Errorf("точка %q: %s утёк в CH-атрибуты", p.Name, hostmetric.AgentVersionAttr)
		}
		if p.Name == agent.DroppedPointsMetric {
			dropped = &sink.points[i]
		}
	}
	for _, name := range hostmetric.AllMetrics() {
		if !gotNames[name] {
			t.Errorf("метрика %q из hostmetric.AllMetrics() не пришла в sink", name)
		}
	}

	// Единственное место во всём дереве, где путь агента до сервера проверяется
	// целиком — потеря обязана не только формироваться, но и доезжать.
	if dropped == nil {
		t.Fatalf("%s не пришла в sink — метрика потерь не доехала до сервера", agent.DroppedPointsMetric)
	}
	if dropped.Type != metric.TypeSum {
		t.Errorf("%s: Type = %q, want %q", agent.DroppedPointsMetric, dropped.Type, metric.TypeSum)
	}
	if !dropped.Monotonic {
		t.Errorf("%s: Monotonic = false, want true", agent.DroppedPointsMetric)
	}
}
