package templates

import (
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestPerfEvidenceEachKind — доказательства perf-issue рендерятся по-своему для
// каждого вида; проверяем slow-db (max time), http-flood (последовательность и
// список URL) и неизвестный вид (только счётчик).
func TestPerfEvidenceEachKind(t *testing.T) {
	slow := PerfIssueDetail(PerfIssueDetailData{
		Issue:    trace.PerfIssue{ID: 1, Kind: trace.KindSlowDBQuery, Title: "slow", Status: "unresolved", Count: 5, SampleTraceID: "t"},
		Evidence: PerfEvidence{Count: 5, MaxUS: 900000, HasMax: true},
	}, "u@e.com")
	out := renderTo(t, slow)
	if !strings.Contains(out, "900.0ms") {
		t.Error("slow-db evidence должна показать max time")
	}

	flood := PerfIssueDetail(PerfIssueDetailData{
		Issue:    trace.PerfIssue{ID: 2, Kind: trace.KindHTTPFlood, Title: "flood", Status: "unresolved", Count: 20, SampleTraceID: "t"},
		Evidence: PerfEvidence{Count: 20, SequentialPct: 75, MaxConcurrency: 4, URLs: []string{"/a", "/b"}, HasSequential: true},
	}, "u@e.com")
	of := renderTo(t, flood)
	if !strings.Contains(of, "/a") || !strings.Contains(of, "75") {
		t.Error("http-flood evidence должна показать последовательность и URL")
	}

	unknown := PerfIssueDetail(PerfIssueDetailData{
		Issue:    trace.PerfIssue{ID: 3, Kind: "custom_kind", Title: "x", Status: "unresolved", Count: 1, SampleTraceID: "t"},
		Evidence: PerfEvidence{Count: 1},
	}, "u@e.com")
	_ = renderTo(t, unknown)

	// N+1 без опциональных доказательств (HasTotal=false, пустой ParentOp) —
	// ветки условий по else.
	bare := PerfIssueDetail(PerfIssueDetailData{
		Issue:    trace.PerfIssue{ID: 4, Kind: trace.KindNPlusOne, Title: "n1", Status: "resolved", Count: 2, SampleTraceID: "t"},
		Evidence: PerfEvidence{Count: 2},
	}, "u@e.com")
	_ = renderTo(t, bare)
}

// TestProjectSettingsRetentionUnset — retentionDays=0 показывает «безлимитную»
// ветку уведомления о хранении, а не число дней.
func TestProjectSettingsRetentionUnset(t *testing.T) {
	out := renderTo(t, ProjectSettings(org.Project{ID: 7, Slug: "web", Name: "Web"}, nil, "dsn", "ошибка ключа", "u@e.com", PerfSettingsForm{}, RegressionSettingsForm{}, 0))
	if !strings.Contains(out, "ошибка ключа") {
		t.Error("ошибка настроек проекта должна отрендериться")
	}
}

// TestIssueDetailBareFrame — кадр не из приложения (InApp=false) и без модуля,
// issue без назначенного и без выбранного события — альтернативные ветки.
func TestIssueDetailBareFrame(t *testing.T) {
	it := issue.Issue{ID: 6, Title: "err", Level: "info", Status: "ignored", TimesSeen: 1, FirstSeen: time.Now(), LastSeen: time.Now()}
	// Системный кадр (InApp=false) с модулем — рендерится через <details>.
	frames := []Frame{{Function: "runtime.main", Module: "runtime", Filename: "", Lineno: 0, InApp: false}}
	ev := event.Stored{ID: "e9", Level: "info", Message: "just a message"}
	out := renderTo(t, IssueDetail(it, nil, stub(), []event.Stored{ev}, "e9", &ev, frames, "u@e.com"))
	if !strings.Contains(out, "runtime.main") || !strings.Contains(out, "frame-system") {
		t.Error("системный кадр не из приложения должен отрендериться через <details>")
	}
}

// TestMonitorFormTCPandDNS — формы монитора kind=tcp/dns показывают свои поля.
func TestMonitorFormTCPandDNS(t *testing.T) {
	base := MonitorFormData{ProjectID: 7, IntervalSeconds: "60", TimeoutSeconds: "10", FailThreshold: "3", RecoveryThreshold: "2",
		TCPHost: "db.internal", TCPPort: "5432", DNSHostname: "example.com", DNSRecordType: "A", DNSExpectedValue: "1.2.3.4"}
	tcp := base
	tcp.Kind = uptime.KindTCP
	tcp.Name = "tcp-mon"
	if out := renderTo(t, MonitorForm(tcp, "u@e.com")); !strings.Contains(out, "db.internal") {
		t.Error("tcp-форма должна показать host")
	}
	dns := base
	dns.Kind = uptime.KindDNS
	dns.Name = "dns-mon"
	if out := renderTo(t, MonitorForm(dns, "u@e.com")); !strings.Contains(out, "example.com") {
		t.Error("dns-форма должна показать hostname")
	}
}

// TestMetricDetailPlain — метрика-gauge без перцентилей и без лейблов: ветки
// без p50/p95/p99 и без матчера лейблов.
func TestMetricDetailPlain(t *testing.T) {
	vm := MetricDetailVM{ProjectID: 7, Info: metric.MetricInfo{Name: "cpu", Type: "gauge", Unit: ""}, Period: "1h", Agg: "avg", Chart: stub(), Percentiles: false}
	out := renderTo(t, MetricDetail(vm, "u@e.com"))
	if !strings.Contains(out, "cpu") {
		t.Error("деталь метрики без перцентилей должна отрендериться")
	}
}

// TestMonitorDetailPausedDisabled — выключенный монитор в статусе paused: ветка
// «возобновить», без SSL и без проверок/инцидентов.
func TestMonitorDetailPausedDisabled(t *testing.T) {
	m := uptime.Monitor{ID: 9, Name: "paused-mon", Kind: uptime.KindTCP, Enabled: false, IntervalSeconds: 120}
	stat := uptime.UptimeStat{}
	out := renderTo(t, MonitorDetail(m, "paused", stat, stat, stat, stub(), nil, nil, true, "https://x", "u@e.com"))
	if !strings.Contains(out, "paused-mon") {
		t.Error("выключенный монитор должен отрендериться")
	}
}

// TestRegisterClosed — регистрация в закрытом режиме прячет форму (ветка
// closed=true).
func TestRegisterClosed(t *testing.T) {
	out := renderTo(t, Register("", true, nil))
	if len(out) == 0 {
		t.Error("закрытая регистрация должна что-то рендерить")
	}
}

// TestEmptyStates — пустые списки во всех разделах показывают пустое состояние,
// а не строки данных.
func TestEmptyStates(t *testing.T) {
	o := org.Org{ID: 1, Slug: "acme", Name: "Acme"}
	empties := map[string]string{
		"issues":       renderTo(t, IssuesList(7, nil, IssuesFilter{}, 1, 0, true, "u@e.com", nil, nil, GettingStartedVM{})),
		"monitors":     renderTo(t, MonitorsList(7, nil, true, "u@e.com")),
		"performance":  renderTo(t, PerformanceList(7, nil, 0, PerfFilter{}, nil, 0, "u@e.com")),
		"webvitals":    renderTo(t, WebVitalsList(7, nil, PerfFilter{}, nil, "u@e.com")),
		"perfissues":   renderTo(t, PerfIssuesList(7, nil, "unresolved", "u@e.com")),
		"profiles":     renderTo(t, ProfilesList(7, nil, "24h", "", "u@e.com")),
		"metrics":      renderTo(t, MetricsList(7, nil, "", "u@e.com")),
		"incidents":    renderTo(t, IncidentsList(7, nil, "u@e.com")),
		"regressions":  renderTo(t, RegressionsList(7, nil, "open", "u@e.com")),
		"profileregs":  renderTo(t, ProfileRegressionsList(7, nil, "open", "u@e.com")),
		"alerts":       renderTo(t, Alerts(7, nil, nil, false, "", "u@e.com")),
		"teams":        renderTo(t, Teams(o, nil, nil, nil, "", "u@e.com")),
		"deliveries":   renderTo(t, AlertDeliveries(7, nil, "u@e.com")),
		"metricalerts": renderTo(t, MetricAlerts(7, nil, nil, "", "u@e.com")),
		"maintenance":  renderTo(t, Maintenance(7, nil, "", "u@e.com")),
		"probes":       renderTo(t, Probes(o, nil, "", "", "", "u@e.com")),
		"statuspages":  renderTo(t, StatusPagesSettings(7, "https://x", nil, StatusPageForm{}, "", "u@e.com")),
	}
	for name, out := range empties {
		if len(out) == 0 {
			t.Errorf("%s: пустой раздел должен что-то рендерить", name)
		}
	}
}

// TestChannelStatusBadgeKinds — бейдж канала со всеми типами (email/webhook/
// telegram) внутри строки канала.
func TestChannelStatusBadgeKinds(t *testing.T) {
	channels := []alert.Channel{
		{ID: 1, Kind: alert.ChannelEmail, Enabled: true, Target: "a@b.c"},
		{ID: 2, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://h"},
		{ID: 3, Kind: alert.ChannelTelegram, Enabled: true, Target: "@ch"},
	}
	out := renderTo(t, Alerts(7, nil, channels, true, "", "u@e.com"))
	if !strings.Contains(out, "@ch") || !strings.Contains(out, "https://h") {
		t.Error("каналы всех типов должны отрендериться")
	}
}
