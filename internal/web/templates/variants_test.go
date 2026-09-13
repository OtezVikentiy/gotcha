package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

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

	bare := PerfIssueDetail(PerfIssueDetailData{
		Issue:    trace.PerfIssue{ID: 4, Kind: trace.KindNPlusOne, Title: "n1", Status: "resolved", Count: 2, SampleTraceID: "t"},
		Evidence: PerfEvidence{Count: 2},
	}, "u@e.com")
	_ = renderTo(t, bare)
}

func TestProjectSettingsRetentionUnset(t *testing.T) {
	out := renderTo(t, ProjectSettings(org.Project{ID: 7, Slug: "web", Name: "Web"}, nil, "ошибка ключа", "u@e.com", PerfSettingsForm{}, RegressionSettingsForm{}, 0, nil))
	if !strings.Contains(out, "ошибка ключа") {
		t.Error("ошибка настроек проекта должна отрендериться")
	}
}

func TestIssueDetailBareFrame(t *testing.T) {
	it := issue.Issue{ID: 6, Title: "err", Level: "info", Status: "ignored", TimesSeen: 1, FirstSeen: time.Now(), LastSeen: time.Now()}
	frames := []Frame{{Function: "runtime.main", Module: "runtime", Filename: "", Lineno: 0, InApp: false}}
	ev := event.Stored{ID: "e9", Level: "info", Message: "just a message"}
	out := renderTo(t, IssueDetail(it, nil, stub(), TimeRangeVM{Key: "24h"}, []event.Stored{ev}, "e9", &ev, frames, "u@e.com", false, false, "", "", true, true, false, 90))
	if !strings.Contains(out, "runtime.main") || !strings.Contains(out, "frame-system") {
		t.Error("системный кадр не из приложения должен отрендериться через <details>")
	}
}

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

func TestMonitorFormSecretKeyInsecureWarning(t *testing.T) {
	warnText := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "secret.insecure_warning")

	insecure := MonitorFormData{ProjectID: 7, Kind: uptime.KindHTTP, Name: "http-mon", HTTPMethod: "GET", SecretKeyInsecure: true}
	if out := renderTo(t, MonitorForm(insecure, "u@e.com")); !strings.Contains(out, warnText) {
		t.Error("на dev-ключе предупреждение о заголовках HTTP-монитора отсутствует")
	}

	secure := insecure
	secure.SecretKeyInsecure = false
	if out := renderTo(t, MonitorForm(secure, "u@e.com")); strings.Contains(out, warnText) {
		t.Error("на сильном ключе предупреждение о заголовках HTTP-монитора не должно рендериться")
	}
}

func TestMetricDetailPlain(t *testing.T) {
	vm := MetricDetailVM{ProjectID: 7, Info: metric.MetricInfo{Name: "cpu", Type: "gauge", Unit: ""}, Range: TimeRangeVM{Key: "1h"}, Agg: "avg", Chart: stub(), Percentiles: false}
	out := renderTo(t, MetricDetail(vm, "u@e.com"))
	if !strings.Contains(out, "cpu") {
		t.Error("деталь метрики без перцентилей должна отрендериться")
	}
}

func TestMonitorDetailPausedDisabled(t *testing.T) {
	m := uptime.Monitor{ID: 9, Name: "paused-mon", Kind: uptime.KindTCP, Enabled: false, IntervalSeconds: 120}
	stat := uptime.UptimeStat{}
	out := renderTo(t, MonitorDetail(m, "paused", stat, stat, stat, stub(), TimeRangeVM{Key: "24h"}, nil, nil, 1, 0, true, true, "https://x", "u@e.com", false))
	if !strings.Contains(out, "paused-mon") {
		t.Error("выключенный монитор должен отрендериться")
	}
}

// проверка по тексту заголовка/тела, не len(out)==0 — та не ловит перепутанные ветки closed/invite.
func TestRegisterClosed(t *testing.T) {
	out := renderTo(t, RegisterStub("", "closed", "", nil))

	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	wantTitle := i18n.T(ctx, "auth.register.closed_title")
	wantBody := i18n.T(ctx, "auth.register.closed_body")
	if !strings.Contains(out, wantTitle) {
		t.Errorf("закрытая регистрация: нет заголовка закрытой регистрации %q", wantTitle)
	}
	if !strings.Contains(out, wantBody) {
		t.Errorf("закрытая регистрация: нет сообщения о закрытой регистрации %q", wantBody)
	}
	// action="/register" — маркер именно формы регистрации, не chromeless-обвязки: та рисует свои формы.
	if strings.Contains(out, `action="/register"`) {
		t.Error("закрытая регистрация не должна показывать форму регистрации")
	}
}

// класс empty-state, не len(out) — страница всегда рендерит layout/навигацию, длина не различает состояния.
const emptyStateMarker = `class="empty-state"`

// падение по конкретному ключу — сигнал, что из этого шаблона убрали @emptyState, не общая деградация.
func TestEmptyStates(t *testing.T) {
	o := org.Org{ID: 1, Slug: "acme", Name: "Acme"}
	empties := map[string]string{
		"issues":       renderTo(t, IssuesList(7, nil, IssuesFilter{}, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, false, false, false)),
		"monitors":     renderTo(t, MonitorsList(7, nil, true, "u@e.com", false)),
		"performance":  renderTo(t, PerformanceList(7, nil, 0, PerfFilter{}, nil, 0, nil, "u@e.com", false, false)),
		"webvitals":    renderTo(t, WebVitalsList(7, nil, PerfFilter{}, nil, "u@e.com", false)),
		"perfissues":   renderTo(t, PerfIssuesList(7, nil, "unresolved", "u@e.com")),
		"profiles":     renderTo(t, ProfilesList(7, nil, TimeRangeVM{Key: "24h"}, "", "u@e.com", false)),
		"metrics":      renderTo(t, MetricsList(7, nil, "", "u@e.com", false, 0, false)),
		"incidents":    renderTo(t, IncidentsList(7, nil, 1, 0, "u@e.com")),
		"regressions":  renderTo(t, RegressionsList(7, nil, nil, "open", "u@e.com", false, true)),
		"profileregs":  renderTo(t, ProfileRegressionsList(7, nil, "open", "u@e.com", true)),
		"alerts":       renderTo(t, Alerts(7, nil, nil, false, true, false, nil, "", "u@e.com")),
		"teams":        renderTo(t, Teams(o, nil, nil, nil, nil, "", "u@e.com")),
		"deliveries":   renderTo(t, AlertDeliveries(7, nil, true, "u@e.com")),
		"metricalerts": renderTo(t, MetricAlerts(7, nil, nil, nil, nil, "", "u@e.com")),
		"maintenance":  renderTo(t, Maintenance(7, nil, nil, "", "u@e.com")),
		"probes":       renderTo(t, Probes(o, nil, "", "", "", "u@e.com")),
		"statuspages":  renderTo(t, StatusPagesSettings(7, "https://x", nil, StatusPageForm{}, true, "", "u@e.com")),
	}
	for name, out := range empties {
		if !strings.Contains(out, emptyStateMarker) {
			t.Errorf("%s: пустой раздел не показывает единое пустое состояние (%s) — удалили @emptyState или сменили класс", name, emptyStateMarker)
		}
	}
}

func TestChannelStatusBadgeKinds(t *testing.T) {
	channels := []alert.Channel{
		{ID: 1, Kind: alert.ChannelEmail, Enabled: true, Target: "a@b.c"},
		{ID: 2, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://h"},
		{ID: 3, Kind: alert.ChannelTelegram, Enabled: true, Target: "@ch"},
	}
	out := renderTo(t, Alerts(7, nil, channels, true, true, false, nil, "", "u@e.com"))
	if !strings.Contains(out, "@ch") || !strings.Contains(out, "https://h") {
		t.Error("каналы всех типов должны отрендериться")
	}
}
