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
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// fakeAgentSample — полный Sample (все секции заполнены, включая CPU-дельту,
// которой у Collector не бывает на первом тике — см. collect.go) для
// e2e-проверки: агент должен эмитить ровно ту же форму данных, что и
// коллектор hostmetrics (hostmetric.AllMetrics()).
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

// TestAgentExportEndToEnd: agent.BuildExport+EncodeBody (T5) → gzip POST в
// реальный Handler.otlpMetrics — тем же путём, каким на проде шлёт
// OTel-коллектор hostmetrics (postOTLPMetrics, otlp_test.go:1492, но с
// Content-Encoding: gzip, как реально шлёт Sender, sender.go). Приёмник
// должен получить точки ВСЕХ метрик hostmetric.AllMetrics(), host.name —
// промоутирован в MetricPoint.Host, а служебный gotcha.agent.version
// (resource-атрибут агента, не datapoint-атрибут) в CH-атрибуты точек не
// попадает — контракт §1.3 спеки: агент неотличим по форме данных от
// коллектора hostmetrics.
func TestAgentExportEndToEnd(t *testing.T) {
	sink := &collectMetricSink{}
	h := NewHandler(NewKeyCache(stubKeyResolver{key: org.Key{ProjectID: 1, OrgID: 1, Kind: org.KindAgent}}), nil, nil, 1<<20)
	h.Metrics = sink

	md := agent.BuildExport("web-1", "", "", fakeAgentSample())
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
	for _, p := range sink.points {
		gotNames[p.Name] = true
		if p.Host != "web-1" {
			t.Errorf("точка %q: Host = %q, want web-1 (host.name должен быть промоутирован)", p.Name, p.Host)
		}
		if _, leaked := p.Attributes[hostmetric.AgentVersionAttr]; leaked {
			t.Errorf("точка %q: %s утёк в CH-атрибуты", p.Name, hostmetric.AgentVersionAttr)
		}
	}
	for _, name := range hostmetric.AllMetrics() {
		if !gotNames[name] {
			t.Errorf("метрика %q из hostmetric.AllMetrics() не пришла в sink", name)
		}
	}
}
