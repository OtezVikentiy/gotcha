package templates

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	templruntime "github.com/a-h/templ/runtime"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestMain уменьшает внутренний буфер templ до 1 байта на весь прогон пакета.
// templ пишет через bufio (по умолчанию 4КБ) и отдаёт ошибку нижележащего
// writer'а только после сброса; с однобайтовым буфером каждая запись сбрасывается
// сразу, что позволяет проверить распространение ошибок writer'а на каждой
// границе записи (см. TestRenderPropagatesWriteErrors). На корректность вывода
// это не влияет — bufio лишь чаще сбрасывается.
func TestMain(m *testing.M) {
	templruntime.DefaultBufferSize = 1
	os.Exit(m.Run())
}

var errWrite = errors.New("write failed")

// failAfter — writer, успешно принимающий ровно n байт, затем возвращающий
// ошибку (с частичной записью на переходной записи). tripped фиксирует, что
// ошибка реально была отдана хотя бы раз — иначе проверять распространение
// нечего (см. TestRenderPropagatesWriteErrors).
type failAfter struct {
	n       int
	tripped bool
}

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		f.tripped = true
		return 0, errWrite
	}
	if len(p) <= f.n {
		f.n -= len(p)
		return len(p), nil
	}
	w := f.n
	f.n = 0
	f.tripped = true
	return w, errWrite
}

// pageComponents — конструкторы всех страничных компонентов с заполненными
// доменными данными. Каждый компонент рендерит layout и дерево под-компонентов,
// поэтому проверка ошибок writer'а на них покрывает и вложенные шаблоны.
func pageComponents() map[string]templ.Component {
	now := time.Now()
	stubC := templ.Raw("<svg data-c></svg>")

	issueRows := []IssueRow{
		{Issue: issue.Issue{ID: 1, Title: "boom", Level: "error", Status: "unresolved", TimesSeen: 9, LastSeen: now, AssigneeEmail: "a@b.c"}, Sparkline: stubC},
		{Issue: issue.Issue{ID: 2, Title: "warn", Level: "warning", Status: "resolved", LastSeen: now}, Sparkline: stubC},
	}
	ev := event.Stored{ID: "e1", Level: "error", ExceptionType: "NPE", ExceptionValue: "nil", Environment: "production", TraceID: "tr", Tags: map[string]string{"k": "v"}}
	members := []org.Member{{UserID: 1, Email: "o@x.io", Role: org.RoleOwner}, {UserID: 2, Email: "m@x.io", Role: org.RoleMember}}
	o := org.Org{ID: 1, Slug: "acme", Name: "Acme", EventQuota: 1000}

	m := map[string]templ.Component{
		"IssuesList":  IssuesList(7, issueRows, IssuesFilter{Status: "unresolved"}, 1, 2, true, "u@e.com", []string{"production"}, &QuotaBanner{Text: "лимит", Href: "/x"}, GettingStartedVM{ProjectID: 7, Done: 2, Step2Done: true}),
		"IssueDetail": IssueDetail(issue.Issue{ID: 5, Title: "NPE", Level: "error", Status: "unresolved", TimesSeen: 3, FirstSeen: now, LastSeen: now, AssigneeID: func() *int64 { v := int64(2); return &v }(), AssigneeEmail: "m@x.io"}, members, stubC, []event.Stored{ev}, "e1", &ev, []Frame{{Function: "f", Filename: "a.go", Lineno: 3, InApp: true}}, "u@e.com", false, false),
		"PerformanceList": PerformanceList(7, []EndpointRow{{Stat: trace.EndpointStat{Transaction: "GET /", Count: 10, Throughput: 5, P50: 1000, P95: 5000, FailureRate: 0.01, ApdexScore: 0.9, Environments: []string{"production"}}, Sparkline: stubC}}, 1, PerfFilter{Period: "24h", Sort: "throughput"}, []string{"production"}, 500, "u@e.com"),
		"EndpointDetail": EndpointDetail(EndpointDetailData{ProjectID: 7, Transaction: "GET /", Period: "24h", Environment: "production", ApdexT: 500, LatencyChart: stubC, Throughput: stubC, Histogram: stubC, StepLabel: "1h", Slowest: []trace.TraceRow{{TraceID: "t", DurationUS: 1000, Timestamp: now, Status: "ok"}}, PerfIssues: []trace.PerfIssue{{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Status: "unresolved", Count: 3}}, Vitals: []VitalPanelRow{{Vital: trace.Vital{Name: "lcp", P75: 2400, Rating: "good", Count: 5}, Chart: stubC}}}, "u@e.com"),
		"MonitorDetail": MonitorDetail(uptime.Monitor{ID: 3, Name: "api", Kind: uptime.KindHTTP, Enabled: true, IntervalSeconds: 60, SSLExpiresAt: func() *time.Time { t := now.Add(240 * time.Hour); return &t }()}, "up", uptime.UptimeStat{Total: 10, OK: 10}, uptime.UptimeStat{Total: 10, OK: 9}, uptime.UptimeStat{Total: 10, OK: 8}, stubC, []uptime.CheckRow{{Timestamp: now, Region: "eu", OK: true, StatusCode: 200, TotalMs: 100}, {Timestamp: now, Region: "us", OK: false, StatusCode: 500, Error: "x", TotalMs: 900}}, []uptime.Incident{{ID: 1, StartedAt: now.Add(-time.Hour), Cause: "t"}, {ID: 2, StartedAt: now.Add(-3 * time.Hour), ResolvedAt: &now, Cause: "5xx"}}, true, "https://g.example", "u@e.com"),
		"MonitorsList":  MonitorsList(7, []MonitorRow{{Monitor: uptime.Monitor{ID: 1, Name: "web", Kind: uptime.KindHTTP}, Status: "up", Uptime24h: uptime.UptimeStat{Total: 5, OK: 5}, AvgLatencyMs: 80, Bars: stubC, LastChecked: &now}, {Monitor: uptime.Monitor{ID: 2, Name: "db", Kind: uptime.KindTCP}, Status: "down", Bars: stubC}}, true, "u@e.com"),
		"MonitorFormHTTP": MonitorForm(MonitorFormData{ProjectID: 7, Kind: uptime.KindHTTP, Name: "m", IntervalSeconds: "60", TimeoutSeconds: "10", FailThreshold: "3", RecoveryThreshold: "2", AllRegions: []string{"eu"}, SelectedRegions: map[string]bool{"eu": true}, AllChannels: []alert.Channel{{ID: 1, Kind: "email", Target: "a@b.c"}}, SelectedChannels: map[int64]bool{1: true}, HTTPMethod: "GET", HTTPURL: "https://x", HTTPExpectedStatus: "200"}, "u@e.com"),
		"MonitorFormHeartbeat": MonitorForm(MonitorFormData{ProjectID: 7, Kind: uptime.KindHeartbeat, IsEdit: true, MonitorID: 4, Name: "cron", ErrMsg: "err", HeartbeatGraceSeconds: "300", TCPHost: "h", TCPPort: "1", DNSHostname: "d", DNSRecordType: "A"}, "u@e.com"),
		"Alerts":          Alerts(7, []alert.Rule{{ID: 1, Kind: alert.KindNewIssue, Enabled: true}, {ID: 2, Kind: alert.KindSpike, Enabled: false, Threshold: 10, WindowMinutes: 5}}, []alert.Channel{{ID: 1, Kind: "email", Enabled: true, Target: "t@x.io"}, {ID: 2, Kind: "telegram", Enabled: false, Target: "@ch"}}, true, "err", "u@e.com"),
		"OrgSettings":     OrgSettings(o, members, 1, []QuotaVM{{Kind: "События", Field: "event_quota", Usage: 50, Limit: 1000}, {Kind: "Транзакции", Field: "transaction_quota", Usage: 0, Limit: 0}}, true, "err", "https://g/invite/t", SSOSettings{IsOwner: true, CanConfigure: true, Configured: true, Issuer: "https://idp", ClientID: "c", Domain: "x.io", DefaultRole: "member", Enforced: true, RedirectURI: "https://g/sso"}, "o@x.io", &QuotaBanner{Text: "лимит", Href: "/x"}),
		"Teams":           Teams(o, []TeamView{{Team: org.Team{ID: 100, Slug: "core", Name: "Core"}, Members: []org.Member{{UserID: 1, Email: "o@x.io", Role: org.RoleOwner}}, Projects: []org.Project{{ID: 10, Name: "web"}}}}, members, []org.Project{{ID: 10, Name: "web"}, {ID: 20, Name: "api"}}, "err", "u@e.com"),
		"ProfilesList":    ProfilesList(7, []profile.ServiceInfo{{Service: "web", Type: "cpu", Transaction: "GET /", Weight: 2_000_000_000, Unit: "nanoseconds", Samples: 100, Environments: []string{"production"}}, {Service: "api", Type: "alloc_space", Transaction: "POST /", Weight: 5 * 1024 * 1024, Unit: "bytes", Samples: 50}}, "24h", "production", "u@e.com"),
		"ProfileRegList":  ProfileRegressionsList(7, []profile.Regression{{ID: 1, Service: "web", ProfileType: "cpu", Function: "hot()", Status: "open", BaselineShare: 0.1, PeakShare: 0.3, StartedAt: now}, {ID: 2, Service: "api", ProfileType: "heap", Function: "leak()", Status: "resolved", StartedAt: now.Add(-2 * time.Hour), ResolvedAt: &now}}, "open", "u@e.com"),
		"MetricsList":     MetricsList(7, []metric.MetricInfo{{Name: "http.rps", Type: "gauge", Unit: "1/s"}, {Name: "q.depth", Type: "histogram"}}, "production", "u@e.com"),
		"MetricDetail":    MetricDetail(MetricDetailVM{ProjectID: 7, Info: metric.MetricInfo{Name: "http.rps", Type: "histogram", Unit: "ms"}, Period: "24h", Agg: "avg", Environment: "production", Environments: []string{"production", "staging"}, Labels: map[string][]string{"route": {"/a", "/b"}}, LabelKey: "route", LabelValue: "/a", Chart: stubC, Percentiles: true}, "u@e.com"),
		"MetricAlerts":    MetricAlerts(7, []metric.Rule{{ID: 1, MetricName: "http.rps", Aggregation: "avg", Comparator: "gt", Threshold: 100, WindowSeconds: 300, Enabled: true}, {ID: 2, MetricName: "err", Aggregation: "max", Comparator: "lt", Threshold: 0.5, WindowSeconds: 60, Environment: "production", LabelKey: "route", LabelValue: "/a"}}, []metric.Incident{{ID: 1, RuleID: 1, Status: "open", PeakValue: 150, CurrentValue: 120, StartedAt: now}, {ID: 2, RuleID: 2, Status: "resolved", StartedAt: now.Add(-2 * time.Hour), ResolvedAt: &now}}, "err", "u@e.com"),
		"WebVitalsList":   WebVitalsList(7, []trace.PageVitals{{Transaction: "/home", LCP: trace.Vital{Name: "lcp", P75: 2400, Rating: "good"}, INP: trace.Vital{Name: "inp", P75: 300, Rating: "needs-improvement"}, CLS: trace.Vital{Name: "cls", P75: 0.3, Rating: "poor"}, Count: 100, Environments: []string{"production"}}}, PerfFilter{Period: "24h"}, []string{"production"}, "u@e.com"),
		"PerfIssuesList":  PerfIssuesList(7, []trace.PerfIssue{{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Culprit: "db", Status: "unresolved", Count: 12, FirstSeen: now, LastSeen: now, SampleTraceID: "t1"}, {ID: 2, Kind: trace.KindSlowDBQuery, Title: "slow", Status: "resolved", SampleTraceID: "t2"}}, "unresolved", "u@e.com"),
		"PerfIssueDetail": PerfIssueDetail(PerfIssueDetailData{Issue: trace.PerfIssue{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Culprit: "db", Status: "unresolved", Count: 12, SampleTraceID: "t1"}, Evidence: PerfEvidence{Count: 12, TotalUS: 1000, MaxUS: 300, ParentOp: "http", SequentialPct: 80, MaxConcurrency: 1, URLs: []string{"/a"}, HasTotal: true, HasMax: true, HasSequential: true}, CanManage: true}, "u@e.com"),
		"IncidentsList":   IncidentsList(7, []IncidentRow{{Incident: uptime.Incident{ID: 1, StartedAt: now.Add(-time.Hour), Cause: "t"}, MonitorName: "web"}, {Incident: uptime.Incident{ID: 2, StartedAt: now.Add(-5 * time.Hour), ResolvedAt: &now, Cause: "5xx"}, MonitorName: "api"}}, "u@e.com"),
		"RegressionsList": RegressionsList(7, []trace.Regression{{ID: 1, TargetKind: "endpoint_p95", Target: "GET /", Metric: "duration", Status: "open", BaselineValue: 100, PeakValue: 300, StartedAt: now}, {ID: 2, TargetKind: "webvital_p75", Target: "/home", Metric: "lcp", Status: "resolved", BaselineValue: 2000, PeakValue: 4000, StartedAt: now.Add(-2 * time.Hour), ResolvedAt: &now}}, "open", "u@e.com"),
		"Probes":          Probes(o, []ProbeRow{{Probe: uptime.Probe{ID: 1, Region: "eu", Name: "eu-1", LastSeenAt: &now}, Status: "online"}, {Probe: uptime.Probe{ID: 2, Region: "us", Name: "us-1"}, Status: "offline"}}, "tok", "run", "", "u@e.com"),
		"ProjectSettings": ProjectSettings(org.Project{ID: 7, OrgID: 1, Slug: "web", Name: "Web", Platform: "go"}, []org.Key{{ID: 1, PublicKey: "pk", Revoked: false}, {ID: 2, PublicKey: "old", Revoked: true}}, "https://dsn", "", "u@e.com", PerfSettingsForm{SampleRate: "1", ApdexMS: "500", NPlusOneMin: "5", SlowDBMs: "300"}, RegressionSettingsForm{ThresholdPct: "20", RecoveryPct: "10", WindowMinutes: "60", MinSamples: "100", Enabled: true}, 30),
		"ProjectsList":    ProjectsList([]ProjectListItem{{Project: org.Project{ID: 1, Name: "web", Slug: "web", Platform: "go"}, CanManage: true}, {Project: org.Project{ID: 2, Name: "api", Slug: "api", Platform: "php"}, CanManage: false}}, "u@e.com"),
		"Onboarding":      Onboarding("err", "acme", "Acme", "web", "Web", "go", "u@e.com"),
		"AlertDeliveries": AlertDeliveries(7, []notify.FailedJob{{ID: 1, ChannelKind: "email", Target: "a@b.c", LastError: strings.Repeat("x", 400), Attempts: 5, CreatedAt: now}}, "u@e.com"),
		"Profile":         Profile("u@e.com", "", "ok", true, []LinkedIdentity{{Provider: "yandex", DisplayName: "Я", Email: "u@ya.ru", CanUnlink: true}}, []LinkableProvider{{Name: "github", DisplayName: "GitHub"}}, "u@e.com"),
		"Maintenance":     Maintenance(7, []uptime.Window{{ID: 1, Name: "one", StartsAt: &now, EndsAt: &now, Timezone: "UTC"}, {ID: 2, Name: "wk", Weekly: true, Weekday: 1, StartTime: "02:00", EndTime: "04:00", Timezone: "Europe/Moscow"}}, "err", "u@e.com"),
		"ProjectSetup":    ProjectSetup(org.Project{ID: 7, Slug: "web", Name: "Web", Platform: "go"}, "https://dsn", "go", "php", "js", "u@e.com"),
		"StatusPagesSet":  StatusPagesSettings(7, "https://g.example", []StatusPageForm{{ID: 1, Slug: "p", Title: "T", Description: "d", Enabled: true, Monitors: []StatusPageFormMonitor{{ID: 10, MonitorName: "web", Selected: true, DisplayName: "W"}, {ID: 20, MonitorName: "api"}}}}, StatusPageForm{Monitors: []StatusPageFormMonitor{{ID: 10, MonitorName: "web"}}}, "err", "u@e.com"),
		"PublicStatus":    PublicStatusPage(StatusPageView{Title: "S", Description: "d", Overall: "partial", Monitors: []StatusMonitorView{{Name: "web", Status: "up", Uptime90d: uptime.UptimeStat{Total: 100, OK: 99}, Bars: stubC}, {Name: "api", Status: "down", Bars: stubC}}, Incidents: []StatusIncidentView{{Name: "I", StartedAt: "t", Ongoing: true}, {Name: "J", StartedAt: "t", Duration: "2h"}}, Maintenance: []StatusWindowView{{Name: "M", From: "a", To: "b"}}}),
		"ProfileFlame":    ProfileFlame(ProfileFlameVM{ProjectID: 7, Service: "web", Type: "cpu", Transaction: "GET /", Environment: "production", Period: "24h", Chart: stubC}, "u@e.com"),
		"TraceWaterfall":  TraceWaterfall(TraceWaterfallData{ProjectID: 7, TraceID: "tr", Transaction: "GET /", TotalUS: 1000, Timestamp: now, Waterfall: stubC, ShownRows: 5, TotalRows: 10, HasProfile: true, From: "endpoint", FromTransaction: "GET /"}, "u@e.com"),
		"TraceFlame":      TraceFlame(TraceFlameData{TraceID: "tr", Chart: stubC}, "u@e.com"),
		"DocsIndex":       DocsIndex([]DocsGroup{{Key: "docs.group.getting_started", Pages: []docs.Page{{Slug: "q", Group: "docs.group.getting_started", Title: "Q"}}}}, "u@e.com"),
		"DocsPage":        DocsPage("q", "Q", "<p>body</p>", []docs.Page{{Slug: "q", Title: "Q"}}, "u@e.com"),
		"ConfirmPage":     ConfirmPage("T", "M", "OK", "/back", "/do", []HiddenField{{Name: "id", Value: "1"}}, "u@e.com"),
		"ErrorPage":       ErrorPage(404, "нет", "u@e.com"),
		"Login":           Login("err", []OAuthButton{{Name: "yandex", Label: "Я"}}),
		"Register":        Register("", false, []OAuthButton{{Name: "github", Label: "GH"}}),
		"SSOLogin":        SSOLogin("err"),
		"InviteAccept":    InviteAccept("tok", "err", "u@e.com"),
	}
	return m
}

// TestRenderPropagatesWriteErrors — SSR-шаблоны не должны молча глотать ошибку
// записи (например, при обрыве соединения клиентом): на каждой границе записи
// прогоняем рендер в writer, падающий после k байт, и требуем, чтобы ошибка
// всплывала наружу, а не терялась (обрезанный ответ без сигнала об ошибке).
//
// Утверждаем только по факту срабатывания writer'а (fw.tripped): часть шаблонов
// рисует относительное время (time.Now()), поэтому длина вывода между рендерами
// на границе секунды может меняться на байт-другой, и на хвостовых смещениях
// writer иногда не добирает свой лимит. Такой рендер просто не относится к
// проверке — важно, что КАЖДЫЙ рендер, где ошибка записи реально произошла,
// её распространил.
func TestRenderPropagatesWriteErrors(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	for name, comp := range pageComponents() {
		var good strings.Builder
		if err := comp.Render(ctx, &good); err != nil {
			t.Fatalf("%s: базовый рендер упал: %v", name, err)
		}
		full := good.Len()
		for k := 0; k < full; k++ {
			fw := &failAfter{n: k}
			err := comp.Render(ctx, fw)
			if fw.tripped && err == nil {
				t.Fatalf("%s: обрыв записи на %d/%d байт проглочен (ошибка не всплыла)", name, k, full)
			}
		}
	}
}

// TestRenderRespectsCancelledContext — при уже отменённом контексте рендер
// обязан вернуть ошибку контекста, а не рисовать страницу впустую.
func TestRenderRespectsCancelledContext(t *testing.T) {
	base := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	ctx, cancel := context.WithCancel(base)
	cancel()
	for name, comp := range pageComponents() {
		var sb strings.Builder
		if err := comp.Render(ctx, &sb); err == nil {
			t.Errorf("%s: отменённый контекст должен давать ошибку рендера", name)
		}
	}
}
