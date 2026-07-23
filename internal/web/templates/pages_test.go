package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
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

// renderTo — общий помощник: рендерит компонент в русской локали и возвращает
// HTML; фейлит тест на ошибке рендера.
func renderTo(t *testing.T, c templ.Component) string {
	t.Helper()
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := c.Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// stub — тривиальный дочерний компонент (график/спарклайн) для мест, где
// шаблон рендерит переданную templ.Component; nil бы уронил рендер.
func stub() templ.Component { return templ.Raw("<svg data-stub></svg>") }

func ptrTime(t time.Time) *time.Time { return &t }
func ptrInt64(v int64) *int64        { return &v }

// TestIssuesListPopulatedVsEmpty — непустой список рисует строки issue с
// уровнем/статусом, а пустой показывает пустое состояние; canManage=true
// открывает массовые действия.
func TestIssuesListPopulatedVsEmpty(t *testing.T) {
	rows := []IssueRow{
		{Issue: issue.Issue{ID: 1, Title: "boom error", Level: "error", Status: "unresolved", TimesSeen: 42, LastSeen: time.Now().Add(-time.Hour)}, Sparkline: stub()},
		{Issue: issue.Issue{ID: 2, Title: "minor warn", Level: "warning", Status: "resolved", TimesSeen: 3, LastSeen: time.Now().Add(-24 * time.Hour), AssigneeEmail: "dev@x.io"}, Sparkline: stub()},
	}
	gs := GettingStartedVM{ProjectID: 7, OrgID: 1, Done: 3, Step2Done: true}
	out := renderTo(t, IssuesList(7, rows, IssuesFilter{Status: "unresolved"}, 1, 2, true, "u@e.com", []string{"production", "staging"}, &QuotaBanner{Text: "почти лимит", Href: "/x"}, gs))
	if !strings.Contains(out, "boom error") || !strings.Contains(out, "badge-danger") {
		t.Error("список issue должен содержать заголовок и бейдж уровня")
	}
	if !strings.Contains(out, "почти лимит") {
		t.Error("баннер квоты должен отрендериться")
	}

	empty := renderTo(t, IssuesList(7, nil, IssuesFilter{}, 1, 0, false, "u@e.com", nil, nil, GettingStartedVM{}))
	if strings.Contains(empty, "boom error") {
		t.Error("пустой список не должен содержать строк")
	}
}

// TestIssueDetail — деталь issue показывает событие, стектрейс-кадры и
// назначенного; выбранное событие раскрывается.
func TestIssueDetail(t *testing.T) {
	it := issue.Issue{ID: 5, Title: "NPE", Level: "error", Status: "unresolved", Culprit: "svc.Do", TimesSeen: 7, FirstSeen: time.Now().Add(-48 * time.Hour), LastSeen: time.Now(), AssigneeID: ptrInt64(2), AssigneeEmail: "dev@x.io"}
	members := []org.Member{{UserID: 2, Email: "dev@x.io", Role: org.RoleAdmin}}
	ev := event.Stored{ID: "ev1", Level: "error", ExceptionType: "NPE", ExceptionValue: "nil ptr", Environment: "production", Release: "1.2.3", TraceID: "abc", Tags: map[string]string{"k": "v"}}
	frames := []Frame{{Function: "main", Module: "app", Filename: "main.go", Lineno: 10, InApp: true}}
	out := renderTo(t, IssueDetail(it, members, stub(), []event.Stored{ev}, "ev1", &ev, frames, "u@e.com", true, true))
	if !strings.Contains(out, "NPE") || !strings.Contains(out, "main.go:10") {
		t.Error("деталь issue должна показать исключение и локацию кадра")
	}
	if !strings.Contains(out, "dev@x.io") {
		t.Error("должен отрисоваться назначенный")
	}
}

// TestPerformanceList — список эндпойнтов с перцентилями и apdex; пустой даёт
// пустое состояние.
func TestPerformanceList(t *testing.T) {
	rows := []EndpointRow{
		{Stat: trace.EndpointStat{Transaction: "GET /api", Count: 1000, Throughput: 12, P50: 5000, P95: 20000, P99: 50000, FailureRate: 0.02, ApdexScore: 0.95, Environments: []string{"production"}}, Sparkline: stub()},
	}
	out := renderTo(t, PerformanceList(7, rows, 1, PerfFilter{Period: "24h", Sort: "throughput"}, []string{"production"}, 500, "u@e.com"))
	if !strings.Contains(out, "GET /api") {
		t.Error("список должен содержать транзакцию")
	}
	empty := renderTo(t, PerformanceList(7, nil, 0, PerfFilter{}, nil, 0, "u@e.com"))
	if strings.Contains(empty, "GET /api") {
		t.Error("пустой список не должен содержать транзакций")
	}
}

// TestEndpointDetail — деталь эндпойнта с медленными трейсами, perf-issue и
// панелью web-vitals.
func TestEndpointDetail(t *testing.T) {
	d := EndpointDetailData{
		ProjectID: 7, Transaction: "GET /api", Period: "24h", Environment: "production", ApdexT: 500,
		LatencyChart: stub(), Throughput: stub(), Histogram: stub(), StepLabel: "1h",
		Slowest:    []trace.TraceRow{{TraceID: "t1", DurationUS: 120000, Timestamp: time.Now(), Status: "ok"}},
		PerfIssues: []trace.PerfIssue{{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Status: "unresolved", Count: 9}},
		Vitals:     []VitalPanelRow{{Vital: trace.Vital{Name: "lcp", P75: 2400, Rating: "good", Count: 50}, Chart: stub()}},
	}
	out := renderTo(t, EndpointDetail(d, "u@e.com"))
	if !strings.Contains(out, "GET /api") || !strings.Contains(out, "N+1") {
		t.Error("деталь эндпойнта должна показать транзакцию и perf-issue")
	}
}

// TestMonitorDetail — деталь монитора: статус, проверки (ok и fail), открытый
// и закрытый инциденты, права управления.
func TestMonitorDetail(t *testing.T) {
	m := uptime.Monitor{ID: 3, Name: "api", Kind: uptime.KindHTTP, Enabled: true, IntervalSeconds: 60, SSLExpiresAt: ptrTime(time.Now().Add(240 * time.Hour))}
	now := time.Now()
	checks := []uptime.CheckRow{
		{Timestamp: now, Region: "eu", OK: true, StatusCode: 200, TotalMs: 120},
		{Timestamp: now.Add(-time.Minute), Region: "us", OK: false, StatusCode: 500, Error: "boom", TotalMs: 900},
	}
	incidents := []uptime.Incident{
		{ID: 1, StartedAt: now.Add(-2 * time.Hour), Cause: "timeout"},
		{ID: 2, StartedAt: now.Add(-5 * time.Hour), ResolvedAt: ptrTime(now.Add(-4 * time.Hour)), Cause: "5xx"},
	}
	stat := uptime.UptimeStat{Total: 100, OK: 99}
	out := renderTo(t, MonitorDetail(m, "up", stat, stat, stat, stub(), checks, incidents, true, "https://gotcha.example", "u@e.com"))
	if !strings.Contains(out, "api") || !strings.Contains(out, "badge-good") || !strings.Contains(out, "badge-danger") {
		t.Error("деталь монитора должна показать имя и статусы проверок")
	}

	// Без прав управления — рендер не должен падать и остаётся валидным.
	noManage := renderTo(t, MonitorDetail(m, "down", stat, stat, stat, stub(), nil, nil, false, "https://x", "u@e.com"))
	if !strings.Contains(noManage, "api") {
		t.Error("монитор без прав всё равно рендерится")
	}
}

// TestMonitorsList — список мониторов со статусами и правами; пустой список
// даёт пустое состояние.
func TestMonitorsList(t *testing.T) {
	last := time.Now().Add(-2 * time.Minute)
	rows := []MonitorRow{
		{Monitor: uptime.Monitor{ID: 1, Name: "web", Kind: uptime.KindHTTP}, Status: "up", Uptime24h: uptime.UptimeStat{Total: 10, OK: 10}, AvgLatencyMs: 80, Bars: stub(), LastChecked: &last},
		{Monitor: uptime.Monitor{ID: 2, Name: "db", Kind: uptime.KindTCP}, Status: "down", Bars: stub()},
	}
	out := renderTo(t, MonitorsList(7, rows, true, "u@e.com"))
	if !strings.Contains(out, "web") || !strings.Contains(out, "db") {
		t.Error("список мониторов должен содержать имена")
	}
	empty := renderTo(t, MonitorsList(7, nil, false, "u@e.com"))
	if strings.Contains(empty, ">web<") {
		t.Error("пустой список не содержит мониторов")
	}
}

// TestMonitorFormEachKind — форма монитора рендерит поля для каждого типа
// (http/tcp/dns/heartbeat) и режим редактирования с ошибкой.
func TestMonitorFormEachKind(t *testing.T) {
	base := MonitorFormData{
		ProjectID: 7, IntervalSeconds: "60", TimeoutSeconds: "10", FailThreshold: "3", RecoveryThreshold: "2",
		AllRegions: []string{"eu", "us"}, SelectedRegions: map[string]bool{"eu": true},
		AllChannels: []alert.Channel{{ID: 1, Kind: "email", Target: "a@b.c"}}, SelectedChannels: map[int64]bool{1: true},
		HTTPMethod: "GET", HTTPURL: "https://x", HTTPExpectedStatus: "200",
		TCPHost: "db", TCPPort: "5432",
		DNSHostname: "example.com", DNSRecordType: "A", DNSExpectedValue: "1.2.3.4",
		HeartbeatGraceSeconds: "300",
	}
	for _, k := range []uptime.Kind{uptime.KindHTTP, uptime.KindTCP, uptime.KindDNS, uptime.KindHeartbeat} {
		d := base
		d.Kind = k
		d.Name = "mon-" + string(k)
		out := renderTo(t, MonitorForm(d, "u@e.com"))
		if !strings.Contains(out, "mon-"+string(k)) {
			t.Errorf("форма kind=%s должна содержать имя", k)
		}
	}
	// Режим правки с ошибкой.
	d := base
	d.IsEdit = true
	d.MonitorID = 9
	d.Kind = uptime.KindHTTP
	d.ErrMsg = "плохой конфиг"
	out := renderTo(t, MonitorForm(d, "u@e.com"))
	if !strings.Contains(out, "плохой конфиг") {
		t.Error("ошибка формы должна отрендериться")
	}
}

// TestAlerts — правила и каналы алертинга рендерятся с включённостью и целью.
func TestAlerts(t *testing.T) {
	rules := []alert.Rule{
		{ID: 1, Kind: alert.KindNewIssue, Enabled: true},
		{ID: 2, Kind: alert.KindSpike, Enabled: false, Threshold: 100, WindowMinutes: 5},
	}
	channels := []alert.Channel{
		{ID: 1, Kind: "email", Enabled: true, Target: "team@x.io"},
		{ID: 2, Kind: "webhook", Enabled: false, Target: "https://hook"},
	}
	out := renderTo(t, Alerts(7, rules, channels, true, "", "u@e.com"))
	if !strings.Contains(out, "team@x.io") || !strings.Contains(out, "https://hook") {
		t.Error("каналы должны отрендериться")
	}
	// С ошибкой.
	outErr := renderTo(t, Alerts(7, nil, nil, false, "ошибка сохранения", "u@e.com"))
	if !strings.Contains(outErr, "ошибка сохранения") {
		t.Error("ошибка должна отрендериться")
	}
}

// TestOrgSettings — настройки орга с участниками разных ролей, квотами, SSO и
// пригласительной ссылкой; владелец видит опасную зону.
func TestOrgSettings(t *testing.T) {
	o := org.Org{ID: 1, Slug: "acme", Name: "Acme", EventQuota: 100000}
	members := []org.Member{
		{UserID: 1, Email: "owner@x.io", Role: org.RoleOwner},
		{UserID: 2, Email: "admin@x.io", Role: org.RoleAdmin},
		{UserID: 3, Email: "member@x.io", Role: org.RoleMember},
	}
	quotas := []QuotaVM{
		{Kind: "События", Field: "event_quota", Usage: 5000, Limit: 100000},
		{Kind: "Транзакции", Field: "transaction_quota", Usage: 0, Limit: 0},
	}
	sso := SSOSettings{IsOwner: true, CanConfigure: true, Configured: true, Issuer: "https://idp", ClientID: "cid", Domain: "x.io", DefaultRole: "member", Enforced: true, RedirectURI: "https://gotcha/sso"}
	out := renderTo(t, OrgSettings(o, members, 1, quotas, true, "", "https://gotcha/invite/tok", sso, "owner@x.io", nil))
	if !strings.Contains(out, "owner@x.io") || !strings.Contains(out, "admin@x.io") {
		t.Error("участники должны отрендериться")
	}
	if !strings.Contains(out, "https://gotcha/invite/tok") {
		t.Error("пригласительная ссылка должна отрендериться")
	}
	// Не-владелец (uid=2): часть управления скрыта, но рендер валиден.
	out2 := renderTo(t, OrgSettings(o, members, 2, quotas, false, "боом", "", SSOSettings{}, "admin@x.io", &QuotaBanner{Text: "лимит", Href: "/x"}))
	if !strings.Contains(out2, "боом") {
		t.Error("ошибка орга должна отрендериться")
	}
}

// TestTeams — команды с участниками и проектами, доступные для добавления.
func TestTeams(t *testing.T) {
	o := org.Org{ID: 1, Slug: "acme", Name: "Acme"}
	orgMembers := []org.Member{{UserID: 1, Email: "a@x.io", Role: org.RoleOwner}, {UserID: 2, Email: "b@x.io", Role: org.RoleMember}}
	orgProjects := []org.Project{{ID: 10, Name: "web"}, {ID: 20, Name: "api"}}
	teams := []TeamView{
		{Team: org.Team{ID: 100, Slug: "core", Name: "Core"}, Members: []org.Member{{UserID: 1, Email: "a@x.io", Role: org.RoleOwner}}, Projects: []org.Project{{ID: 10, Name: "web"}}},
	}
	out := renderTo(t, Teams(o, teams, orgMembers, orgProjects, "", "u@e.com"))
	if !strings.Contains(out, "Core") || !strings.Contains(out, "web") {
		t.Error("команды и проекты должны отрендериться")
	}
	// Пустой список команд + ошибка.
	outEmpty := renderTo(t, Teams(o, nil, orgMembers, orgProjects, "ошибка", "u@e.com"))
	if !strings.Contains(outEmpty, "ошибка") {
		t.Error("ошибка должна отрендериться")
	}
}

// TestProfilesList — обзор профилей с весами по типам; пустой даёт пустое
// состояние.
func TestProfilesList(t *testing.T) {
	services := []profile.ServiceInfo{
		{Service: "web", Type: "cpu", Transaction: "GET /", Weight: 2_000_000_000, Unit: "nanoseconds", Samples: 1000, Environments: []string{"production"}},
		{Service: "web", Type: "alloc_space", Transaction: "POST /", Weight: 5 * 1024 * 1024, Unit: "bytes", Samples: 500},
	}
	out := renderTo(t, ProfilesList(7, services, "24h", "production", "u@e.com"))
	if !strings.Contains(out, "web") {
		t.Error("список профилей должен содержать сервис")
	}
	empty := renderTo(t, ProfilesList(7, nil, "24h", "", "u@e.com"))
	if strings.Contains(empty, ">GET /<") {
		t.Error("пустой список профилей")
	}
}

// TestProfileRegressionsList — регрессии профилей: открытая и закрытая.
func TestProfileRegressionsList(t *testing.T) {
	now := time.Now()
	regs := []profile.Regression{
		{ID: 1, Service: "web", ProfileType: "cpu", Function: "hot()", Status: "open", BaselineShare: 0.1, PeakShare: 0.3, CurrentShare: 0.25, StartedAt: now.Add(-time.Hour)},
		{ID: 2, Service: "api", ProfileType: "heap", Function: "leak()", Status: "resolved", BaselineShare: 0.05, PeakShare: 0.2, StartedAt: now.Add(-3 * time.Hour), ResolvedAt: ptrTime(now.Add(-time.Hour))},
	}
	out := renderTo(t, ProfileRegressionsList(7, regs, "open", "u@e.com"))
	if !strings.Contains(out, "hot()") {
		t.Error("регрессии профилей должны содержать функцию")
	}
}

// TestMetricsListAndDetail — список метрик и деталь с перцентилями и лейблами.
func TestMetricsListAndDetail(t *testing.T) {
	metrics := []metric.MetricInfo{{Name: "http.rps", Type: "gauge", Unit: "1/s"}, {Name: "queue.depth", Type: "histogram", Unit: ""}}
	out := renderTo(t, MetricsList(7, metrics, "production", "u@e.com"))
	if !strings.Contains(out, "http.rps") {
		t.Error("список метрик должен содержать имя")
	}

	vm := MetricDetailVM{
		ProjectID: 7, Info: metric.MetricInfo{Name: "http.rps", Type: "histogram", Unit: "ms"},
		Period: "24h", Agg: "avg", Environment: "production", Environments: []string{"production", "staging"},
		Labels: map[string][]string{"route": {"/a", "/b"}}, LabelKey: "route", LabelValue: "/a",
		Chart: stub(), Percentiles: true,
	}
	outD := renderTo(t, MetricDetail(vm, "u@e.com"))
	if !strings.Contains(outD, "http.rps") {
		t.Error("деталь метрики должна содержать имя")
	}
}

// TestMetricAlerts — правила метрик со scope и инциденты open/resolved.
func TestMetricAlerts(t *testing.T) {
	now := time.Now()
	rules := []metric.Rule{
		{ID: 1, MetricName: "http.rps", Aggregation: "avg", Comparator: "gt", Threshold: 100, WindowSeconds: 300, Enabled: true},
		{ID: 2, MetricName: "err.rate", Aggregation: "max", Comparator: "lt", Threshold: 0.5, WindowSeconds: 60, Environment: "production", LabelKey: "route", LabelValue: "/a", Enabled: false},
	}
	incidents := []metric.Incident{
		{ID: 1, RuleID: 1, Status: "open", PeakValue: 150, CurrentValue: 120, StartedAt: now.Add(-time.Hour)},
		{ID: 2, RuleID: 2, Status: "resolved", PeakValue: 0.9, StartedAt: now.Add(-2 * time.Hour), ResolvedAt: ptrTime(now.Add(-time.Hour))},
	}
	out := renderTo(t, MetricAlerts(7, rules, incidents, "", "u@e.com"))
	if !strings.Contains(out, "http.rps") || !strings.Contains(out, "err.rate") {
		t.Error("правила метрик должны отрендериться")
	}
}

// TestWebVitalsList — список страниц с рейтингами всех трёх core-vitals;
// пустой даёт пустое состояние.
func TestWebVitalsList(t *testing.T) {
	pages := []trace.PageVitals{
		{Transaction: "/home", LCP: trace.Vital{Name: "lcp", P75: 2400, Rating: "good"}, INP: trace.Vital{Name: "inp", P75: 300, Rating: "needs-improvement"}, CLS: trace.Vital{Name: "cls", P75: 0.3, Rating: "poor"}, Count: 500, Environments: []string{"production"}},
	}
	out := renderTo(t, WebVitalsList(7, pages, PerfFilter{Period: "24h"}, []string{"production"}, "u@e.com"))
	if !strings.Contains(out, "/home") || !strings.Contains(out, "badge-good") || !strings.Contains(out, "badge-danger") {
		t.Error("список web-vitals должен содержать страницу и разные бейджи рейтинга")
	}
	empty := renderTo(t, WebVitalsList(7, nil, PerfFilter{}, nil, "u@e.com"))
	if strings.Contains(empty, "/home") {
		t.Error("пустой web-vitals")
	}
}

// TestPerfIssuesListAndDetail — список perf-issue разных видов и деталь с
// доказательствами (evidence) и правами.
func TestPerfIssuesListAndDetail(t *testing.T) {
	now := time.Now()
	issues := []trace.PerfIssue{
		{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1 query", Culprit: "SELECT users", Status: "unresolved", Count: 12, FirstSeen: now.Add(-time.Hour), LastSeen: now, SampleTraceID: "t1"},
		{ID: 2, Kind: trace.KindSlowDBQuery, Title: "slow query", Status: "resolved", Count: 3, SampleTraceID: "t2"},
	}
	out := renderTo(t, PerfIssuesList(7, issues, "unresolved", "u@e.com"))
	if !strings.Contains(out, "N+1 query") {
		t.Error("список perf-issue должен содержать заголовок")
	}

	d := PerfIssueDetailData{
		Issue:     trace.PerfIssue{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Culprit: "db", Status: "unresolved", Count: 12, SampleTraceID: "t1"},
		Evidence:  PerfEvidence{Count: 12, TotalUS: 120000, MaxUS: 30000, ParentOp: "http.server", SequentialPct: 80, MaxConcurrency: 1, URLs: []string{"/a", "/b"}, HasTotal: true, HasMax: true, HasSequential: true},
		CanManage: true,
	}
	outD := renderTo(t, PerfIssueDetail(d, "u@e.com"))
	if !strings.Contains(outD, "N+1") {
		t.Error("деталь perf-issue должна содержать заголовок")
	}
	// Без прав.
	d.CanManage = false
	_ = renderTo(t, PerfIssueDetail(d, "u@e.com"))
}

// TestIncidentsAndRegressionsLists — списки инцидентов uptime и регрессий perf.
func TestIncidentsAndRegressionsLists(t *testing.T) {
	now := time.Now()
	incRows := []IncidentRow{
		{Incident: uptime.Incident{ID: 1, StartedAt: now.Add(-time.Hour), Cause: "timeout"}, MonitorName: "web"},
		{Incident: uptime.Incident{ID: 2, StartedAt: now.Add(-5 * time.Hour), ResolvedAt: ptrTime(now.Add(-4 * time.Hour)), Cause: "5xx"}, MonitorName: "api"},
	}
	out := renderTo(t, IncidentsList(7, incRows, "u@e.com"))
	if !strings.Contains(out, "web") || !strings.Contains(out, "api") {
		t.Error("инциденты должны содержать имена мониторов")
	}

	regs := []trace.Regression{
		{ID: 1, TargetKind: "endpoint_p95", Target: "GET /api", Metric: "duration", Status: "open", BaselineValue: 100, PeakValue: 300, CurrentValue: 250, StartedAt: now.Add(-time.Hour)},
		{ID: 2, TargetKind: "webvital_p75", Target: "/home", Metric: "lcp", Status: "resolved", BaselineValue: 2000, PeakValue: 4000, StartedAt: now.Add(-3 * time.Hour), ResolvedAt: ptrTime(now.Add(-time.Hour))},
	}
	outR := renderTo(t, RegressionsList(7, regs, "open", "u@e.com"))
	if !strings.Contains(outR, "GET /api") {
		t.Error("регрессии должны содержать цель")
	}
}

// TestProbes — региональные пробы со статусами online/offline и токеном
// новой пробы.
func TestProbes(t *testing.T) {
	now := time.Now()
	rows := []ProbeRow{
		{Probe: uptime.Probe{ID: 1, Region: "eu", Name: "eu-1", LastSeenAt: &now}, Status: "online"},
		{Probe: uptime.Probe{ID: 2, Region: "us", Name: "us-1"}, Status: "offline"},
	}
	out := renderTo(t, Probes(org.Org{ID: 1, Slug: "acme"}, rows, "rawtok123", "gotcha probe run", "", "u@e.com"))
	if !strings.Contains(out, "eu-1") || !strings.Contains(out, "rawtok123") {
		t.Error("пробы и сырой токен должны отрендериться")
	}
}

// TestProjectSettings — настройки проекта с ключами (активный/отозванный),
// формами perf/регрессий и DSN.
func TestProjectSettings(t *testing.T) {
	project := org.Project{ID: 7, OrgID: 1, Slug: "web", Name: "Web", Platform: "go"}
	keys := []org.Key{{ID: 1, PublicKey: "pk_live", Revoked: false}, {ID: 2, PublicKey: "pk_old", Revoked: true}}
	perf := PerfSettingsForm{SampleRate: "1.0", ApdexMS: "500", NPlusOneMin: "5", SlowDBMs: "300"}
	reg := RegressionSettingsForm{ThresholdPct: "20", RecoveryPct: "10", WindowMinutes: "60", MinSamples: "100", Enabled: true}
	out := renderTo(t, ProjectSettings(project, keys, "https://key@dsn", "", "u@e.com", perf, reg, 30))
	if !strings.Contains(out, "pk_live") || !strings.Contains(out, "badge-danger") {
		t.Error("ключи со статусами должны отрендериться")
	}
}

// TestProjectsListAndNoProjects — список проектов и пустой экран без проектов.
func TestProjectsListAndNoProjects(t *testing.T) {
	items := []ProjectListItem{
		{Project: org.Project{ID: 1, Name: "web", Slug: "web", Platform: "go"}, CanManage: true},
		{Project: org.Project{ID: 2, Name: "api", Slug: "api", Platform: "php"}, CanManage: false},
	}
	out := renderTo(t, ProjectsList(items, "u@e.com"))
	if !strings.Contains(out, "web") || !strings.Contains(out, "api") {
		t.Error("список проектов должен содержать имена")
	}
	np := renderTo(t, NoProjects("u@e.com"))
	if len(np) == 0 {
		t.Error("экран без проектов должен что-то рендерить")
	}
}

// TestOnboarding — форма онбординга рендерит переданные значения и ошибку.
func TestOnboarding(t *testing.T) {
	out := renderTo(t, Onboarding("занятый slug", "acme", "Acme", "web", "Web", "go", "u@e.com"))
	if !strings.Contains(out, "занятый slug") || !strings.Contains(out, "Acme") {
		t.Error("онбординг должен показать ошибку и значения")
	}
}

// TestAlertDeliveries — журнал упавших доставок с усечённой ошибкой.
func TestAlertDeliveries(t *testing.T) {
	failed := []notify.FailedJob{
		{ID: 1, ChannelKind: "email", Target: "a@b.c", LastError: strings.Repeat("x", 400), Attempts: 5, CreatedAt: time.Now()},
	}
	out := renderTo(t, AlertDeliveries(7, failed, "u@e.com"))
	if !strings.Contains(out, "a@b.c") {
		t.Error("упавшие доставки должны показать цель")
	}
	empty := renderTo(t, AlertDeliveries(7, nil, "u@e.com"))
	if len(empty) == 0 {
		t.Error("пустой журнал доставок всё равно рендерится")
	}
}

// TestProfilePage — страница профиля пользователя: связанные и подключаемые
// провайдеры, наличие пароля, сообщение и ошибка.
func TestProfilePage(t *testing.T) {
	linked := []LinkedIdentity{{Provider: "yandex", DisplayName: "Яндекс", Email: "u@ya.ru", CanUnlink: true}}
	linkable := []LinkableProvider{{Name: "github", DisplayName: "GitHub"}}
	out := renderTo(t, Profile("u@e.com", "", "сохранено", true, linked, linkable, "u@e.com"))
	if !strings.Contains(out, "Яндекс") || !strings.Contains(out, "GitHub") {
		t.Error("провайдеры должны отрендериться")
	}
	if !strings.Contains(out, "сохранено") {
		t.Error("сообщение должно отрендериться")
	}
	// Без пароля и с ошибкой.
	outErr := renderTo(t, Profile("u@e.com", "ошибка", "", false, nil, linkable, "u@e.com"))
	if !strings.Contains(outErr, "ошибка") {
		t.Error("ошибка профиля должна отрендериться")
	}
}
