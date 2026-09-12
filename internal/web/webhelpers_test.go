package web

import (
	"encoding/json"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestPathHelpers(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{alertsRulesPath(7), "/projects/7/alerts/rules"},
		{alertsChannelsPath(7), "/projects/7/alerts/channels"},
		{alertsChannelsDeletePath(7), "/projects/7/alerts/channels/delete"},
		{incidentsPath(7), "/projects/7/incidents"},
		{maintenanceDeletePath(7), "/projects/7/maintenance/delete"},
		{monitorPausePath(9), "/monitors/9/pause"},
		{monitorResumePath(9), "/monitors/9/resume"},
		{monitorDeletePath(9), "/monitors/9/delete"},
		{metricDetailURL(7, "http.server.duration"), "/projects/7/metrics/http.server.duration"},
		// имя метрики с символами, требующими экранирования пути
		{metricDetailURL(7, "a b/c"), "/projects/7/metrics/a%20b%2Fc"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("path = %q, want %q", c.got, c.want)
		}
	}
}

func TestHeadersToText(t *testing.T) {
	if got := headersToText(nil); got != "" {
		t.Errorf("пустая карта = %q, want пусто", got)
	}
	got := headersToText(map[string]string{"X-B": "2", "X-A": "1", "X-C": "3"})
	if got != "X-A: 1\nX-B: 2\nX-C: 3" {
		t.Errorf("порядок не стабилен: %q", got)
	}
}

func TestIntsToText(t *testing.T) {
	if got := intsToText(nil); got != "" {
		t.Errorf("пусто = %q", got)
	}
	if got := intsToText([]int{200, 201, 204}); got != "200,201,204" {
		t.Errorf("intsToText = %q", got)
	}
}

func TestHeartbeatSnippets(t *testing.T) {
	if got := heartbeatPingURL("https://g.example", "tok123"); got != "https://g.example/uptime/hb/tok123" {
		t.Errorf("pingURL = %q", got)
	}
	// интервал в секундах → минуты, минимум 1
	if got := heartbeatCronSnippet("https://g.example", "tok", 300); !strings.HasPrefix(got, "*/5 * * * * curl") {
		t.Errorf("cron 300s = %q, ожидался шаг 5 минут", got)
	}
	if got := heartbeatCronSnippet("https://g.example", "tok", 30); !strings.HasPrefix(got, "*/1 * * * *") {
		t.Errorf("cron 30s = %q, ожидался минимум 1 минута", got)
	}
	// явный -X POST: голый GET неотличим от префетч-бота/антивирусного прокси.
	if got := heartbeatCronSnippet("https://g.example", "tok", 300); !strings.Contains(got, "curl -fsS -X POST ") {
		t.Errorf("cron snippet = %q, want содержит \"curl -fsS -X POST \"", got)
	}
}

func TestMetricAggFor(t *testing.T) {
	cases := []struct{ typ, agg, want string }{
		{"histogram", "p95", "p95"},
		{"histogram", "avg", "avg"},
		{"histogram", "sum", "p95"}, // sum не для histogram → дефолт p95
		{"gauge", "max", "max"},
		{"gauge", "p95", "avg"}, // перцентиль не для gauge → дефолт avg
		{"counter", "", "avg"},
	}
	for _, c := range cases {
		if got := metricAggFor(c.typ, c.agg); got != c.want {
			t.Errorf("metricAggFor(%q,%q) = %q, want %q", c.typ, c.agg, got, c.want)
		}
	}
}

func TestMonitorFormFromMonitor(t *testing.T) {
	mk := func(kind uptime.Kind, cfg any) uptime.Monitor {
		raw, _ := json.Marshal(cfg)
		return uptime.Monitor{
			ID: 5, ProjectID: 7, Name: "m", Kind: kind, Config: raw,
			IntervalSeconds: 60, TimeoutSeconds: 10, FailThreshold: 1,
			RecoveryThreshold: 1, Consensus: uptime.ConsensusMajority,
			Regions: []string{"local"}, ChannelIDs: []int64{1},
		}
	}

	http := monitorFormFromMonitor(mk(uptime.KindHTTP, uptime.HTTPConfig{
		Method: "POST", URL: "https://x/health", Headers: map[string]string{"A": "1"},
		ExpectedStatus: []int{200, 204}, FollowRedirects: true,
	}))
	if !http.IsEdit || http.HTTPMethod != "POST" || http.HTTPURL != "https://x/health" ||
		http.HTTPHeaders != "A: 1" || http.HTTPExpectedStatus != "200,204" || !http.HTTPFollowRedirects {
		t.Fatalf("HTTP-ветка: %+v", http)
	}

	tcp := monitorFormFromMonitor(mk(uptime.KindTCP, uptime.TCPConfig{Host: "db", Port: 5432}))
	if tcp.TCPHost != "db" || tcp.TCPPort != "5432" {
		t.Fatalf("TCP-ветка: %+v", tcp)
	}

	dns := monitorFormFromMonitor(mk(uptime.KindDNS, uptime.DNSConfig{Hostname: "x.io", RecordType: "AAAA", ExpectedValue: "::1"}))
	if dns.DNSHostname != "x.io" || dns.DNSRecordType != "AAAA" || dns.DNSExpectedValue != "::1" {
		t.Fatalf("DNS-ветка: %+v", dns)
	}

	hb := monitorFormFromMonitor(mk(uptime.KindHeartbeat, uptime.HeartbeatConfig{GraceSeconds: 120}))
	if hb.HeartbeatGraceSeconds != "120" {
		t.Fatalf("Heartbeat-ветка: %+v", hb)
	}
}
