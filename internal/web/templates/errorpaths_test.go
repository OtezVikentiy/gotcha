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
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

// templ фиксирует размер буфера при первом Buffer.Reset из sync.Pool: задавать
// DefaultBufferSize нужно здесь, до первого рендера в процессе — правка позже не подействует.
func TestMain(m *testing.M) {
	templruntime.DefaultBufferSize = 1
	os.Exit(m.Run())
}

var errWrite = errors.New("write failed")

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

func pageComponents() map[string]templ.Component {
	now := time.Now()
	stubC := templ.Raw("<svg data-c></svg>")
	cpu04 := 0.4
	recipe := recipes.All()[0]

	issueRows := []IssueRow{
		{Issue: issue.Issue{ID: 1, Title: "boom", Level: "error", Status: "unresolved", TimesSeen: 9, LastSeen: now, AssigneeEmail: "a@b.c"}, Sparkline: stubC},
		{Issue: issue.Issue{ID: 2, Title: "warn", Level: "warning", Status: "resolved", LastSeen: now}, Sparkline: stubC},
	}
	ev := event.Stored{
		ID: "e1", Level: "error", ExceptionType: "NPE", ExceptionValue: "nil",
		Environment: "production", TraceID: "tr", Tags: map[string]string{"k": "v"},
		Request:     `{"method":"POST","url":"https://x/api","query_string":"a=1&b=2","data":{"name":"bob"},"headers":{"H":"v"}}`,
		Breadcrumbs: `{"values":[{"category":"http","level":"info","message":"GET /x","timestamp":"2026-07-01T00:00:00Z"}]}`,
		Contexts:    `{"os":{"name":"Linux","version":"6"},"runtime":{"name":"go"}}`,
	}
	members := []org.Member{{UserID: 1, Email: "o@x.io", Role: org.RoleOwner}, {UserID: 2, Email: "m@x.io", Role: org.RoleMember}}
	o := org.Org{ID: 1, Slug: "acme", Name: "Acme", EventQuota: 1000}
	group := incidentgroup.GroupRow{
		Group:    incidentgroup.Group{ID: 1, ProjectID: 7, RootSource: "host", RootNodeKind: "host", StartedAt: now},
		RootName: "web-1",
	}

	m := map[string]templ.Component{
		"IssuesList":           IssuesList(7, issueRows, IssuesFilter{Status: "unresolved"}, 1, 2, "u@e.com", []string{"production"}, &QuotaBanner{Text: "лимит", Href: "/x"}, GettingStartedVM{ProjectID: 7, Done: 2, Step2Done: true}, true, true, false),
		"IssueDetail":          IssueDetail(issue.Issue{ID: 5, Title: "NPE", Level: "error", Status: "unresolved", TimesSeen: 3, FirstSeen: now, LastSeen: now, AssigneeID: func() *int64 { v := int64(2); return &v }(), AssigneeEmail: "m@x.io"}, members, stubC, TimeRangeVM{Key: "24h"}, []event.Stored{ev}, "e1", &ev, []Frame{{Function: "f", Filename: "a.go", Lineno: 3, InApp: true}}, "u@e.com", false, false, "", "", true, true, false, 90),
		"PerformanceList":      PerformanceList(7, []EndpointRow{{Stat: trace.EndpointStat{Transaction: "GET /", Count: 10, Throughput: 5, P50: 1000, P95: 5000, FailureRate: 0.01, ApdexScore: 0.9, Environments: []string{"production"}}, Sparkline: stubC}}, 1, PerfFilter{Range: TimeRangeVM{Key: "24h"}, Sort: "throughput"}, []string{"production"}, 500, nil, "u@e.com", false, false),
		"EndpointDetail":       EndpointDetail(EndpointDetailData{ProjectID: 7, Transaction: "GET /", Range: TimeRangeVM{Key: "custom", Custom: true, Start: "2026-07-01T00:00", End: "2026-07-10T00:00"}, Environment: "production", ApdexT: 500, LatencyChart: stubC, Throughput: stubC, Histogram: stubC, StepLabel: "1h", Slowest: []SlowestTraceRow{{Row: trace.TraceRow{TraceID: "t", DurationUS: 1000, Timestamp: now, Status: "ok"}}}, PerfIssues: []trace.PerfIssue{{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Status: "unresolved", Count: 3}}, Vitals: []VitalPanelRow{{Vital: trace.Vital{Name: "lcp", P75: 2400, Rating: "good", Count: 5}, Chart: stubC}}}, "u@e.com"),
		"MonitorDetail":        MonitorDetail(uptime.Monitor{ID: 3, Name: "api", Kind: uptime.KindHTTP, Enabled: true, IntervalSeconds: 60, SSLExpiresAt: func() *time.Time { t := now.Add(240 * time.Hour); return &t }()}, "up", uptime.UptimeStat{Total: 10, OK: 10}, uptime.UptimeStat{Total: 10, OK: 9}, uptime.UptimeStat{Total: 10, OK: 8}, stubC, TimeRangeVM{Key: "24h"}, []uptime.CheckRow{{Timestamp: now, Region: "eu", OK: true, StatusCode: 200, TotalMs: 100}, {Timestamp: now, Region: "us", OK: false, StatusCode: 500, Error: "x", TotalMs: 900}}, []uptime.Incident{{ID: 1, StartedAt: now.Add(-time.Hour), Cause: "t"}, {ID: 2, StartedAt: now.Add(-3 * time.Hour), ResolvedAt: &now, Cause: "5xx"}}, 2, 120, true, "https://g.example", "u@e.com", false),
		"MonitorsList":         MonitorsList(7, []MonitorRow{{Monitor: uptime.Monitor{ID: 1, Name: "web", Kind: uptime.KindHTTP}, Status: "up", Uptime24h: uptime.UptimeStat{Total: 5, OK: 5}, AvgLatencyMs: 80, Bars: stubC, LastChecked: &now}, {Monitor: uptime.Monitor{ID: 2, Name: "db", Kind: uptime.KindTCP}, Status: "down", Bars: stubC}}, true, "u@e.com", false),
		"MonitorFormHTTP":      MonitorForm(MonitorFormData{ProjectID: 7, Kind: uptime.KindHTTP, Name: "m", IntervalSeconds: "60", TimeoutSeconds: "10", FailThreshold: "3", RecoveryThreshold: "2", AllRegions: []string{"eu"}, SelectedRegions: map[string]bool{"eu": true}, AllChannels: []alert.Channel{{ID: 1, Kind: "email", Target: "a@b.c"}}, SelectedChannels: map[int64]bool{1: true}, HTTPMethod: "GET", HTTPURL: "https://x", HTTPExpectedStatus: "200"}, "u@e.com"),
		"MonitorFormHeartbeat": MonitorForm(MonitorFormData{ProjectID: 7, Kind: uptime.KindHeartbeat, IsEdit: true, MonitorID: 4, Name: "cron", ErrMsg: "err", HeartbeatGraceSeconds: "300", TCPHost: "h", TCPPort: "1", DNSHostname: "d", DNSRecordType: "A"}, "u@e.com"),
		"Alerts":               Alerts(7, []alert.Rule{{ID: 1, Kind: alert.KindNewIssue, Enabled: true}, {ID: 2, Kind: alert.KindSpike, Enabled: false, Threshold: 10, WindowMinutes: 5}}, []alert.Channel{{ID: 1, Kind: "email", Enabled: true, Target: "t@x.io"}, {ID: 2, Kind: "telegram", Enabled: false, Target: "@ch"}}, true, true, true, nil, "err", "u@e.com"),
		"OrgSettings":          OrgSettings(o, members, 1, []QuotaVM{{Kind: "События", Field: "event_quota", Usage: 50, Limit: 1000}, {Kind: "Транзакции", Field: "transaction_quota", Usage: 0, Limit: 0}}, true, "err", "https://g/invite/t", SSOSettings{IsOwner: true, CanConfigure: true, Configured: true, Issuer: "https://idp", ClientID: "c", Domain: "x.io", DefaultRole: "member", Enforced: true, RedirectURI: "https://g/sso"}, "o@x.io", &QuotaBanner{Text: "лимит", Href: "/x"}, SubjectPurgeVM{}, nil, nil),
		"Teams":                Teams(o, []TeamView{{Team: org.Team{ID: 100, Slug: "core", Name: "Core"}, Members: []org.Member{{UserID: 1, Email: "o@x.io", Role: org.RoleOwner}}, Projects: []org.Project{{ID: 10, Name: "web"}}}}, members, []org.Project{{ID: 10, Name: "web"}, {ID: 20, Name: "api"}}, nil, "err", "u@e.com"),
		"ProfilesList":         ProfilesList(7, []profile.ServiceInfo{{Service: "web", Type: "cpu", Transaction: "GET /", Weight: 2_000_000_000, Unit: "nanoseconds", Samples: 100, Environments: []string{"production"}}, {Service: "api", Type: "alloc_space", Transaction: "POST /", Weight: 5 * 1024 * 1024, Unit: "bytes", Samples: 50}}, TimeRangeVM{Key: "24h"}, "production", "u@e.com", false),
		"ProfileRegList":       ProfileRegressionsList(7, []profile.Regression{{ID: 1, Service: "web", ProfileType: "cpu", Function: "hot()", Status: "open", BaselineShare: 0.1, PeakShare: 0.3, StartedAt: now}, {ID: 2, Service: "api", ProfileType: "heap", Function: "leak()", Status: "resolved", StartedAt: now.Add(-2 * time.Hour), ResolvedAt: &now}}, "open", "u@e.com", true, map[int64]string{1: "u@e.com"}),
		"MetricsList":          MetricsList(7, []metric.MetricInfo{{Name: "http.rps", Type: "gauge", Unit: "1/s"}, {Name: "q.depth", Type: "histogram"}}, "production", "u@e.com", false, 2, false),
		"MetricDetail":         MetricDetail(MetricDetailVM{ProjectID: 7, Info: metric.MetricInfo{Name: "http.rps", Type: "histogram", Unit: "ms"}, Range: TimeRangeVM{Key: "24h"}, Agg: "avg", Environment: "production", Environments: []string{"production", "staging"}, Labels: map[string][]string{"route": {"/a", "/b"}}, LabelKey: "route", LabelValue: "/a", Chart: stubC, Percentiles: true}, "u@e.com"),
		"MetricAlerts":         MetricAlerts(7, []metric.Rule{{ID: 1, MetricName: "http.rps", Aggregation: "avg", Comparator: "gt", Threshold: 100, WindowSeconds: 300, Enabled: true}, {ID: 2, MetricName: "err", Aggregation: "max", Comparator: "lt", Threshold: 0.5, WindowSeconds: 60, Environment: "production", LabelKey: "route", LabelValue: "/a"}}, []metric.Incident{{ID: 1, RuleID: 1, Status: "open", PeakValue: 150, CurrentValue: 120, StartedAt: now}, {ID: 2, RuleID: 2, Status: "resolved", StartedAt: now.Add(-2 * time.Hour), ResolvedAt: &now}}, []string{"http.rps", "http.server.duration"}, FormState{"metric_name": "http.rps"}, "err", "u@e.com"),
		"WebVitalsList":        WebVitalsList(7, []trace.PageVitals{{Transaction: "/home", LCP: trace.Vital{Name: "lcp", P75: 2400, Rating: "good"}, INP: trace.Vital{Name: "inp", P75: 300, Rating: "needs-improvement"}, CLS: trace.Vital{Name: "cls", P75: 0.3, Rating: "poor"}, Count: 100, Environments: []string{"production"}}}, PerfFilter{Range: TimeRangeVM{Key: "24h"}}, []string{"production"}, "u@e.com", false),
		"PerfIssuesList":       PerfIssuesList(7, []trace.PerfIssue{{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Culprit: "db", Status: "unresolved", Count: 12, FirstSeen: now, LastSeen: now, SampleTraceID: "t1"}, {ID: 2, Kind: trace.KindSlowDBQuery, Title: "slow", Status: "resolved", SampleTraceID: "t2"}}, "unresolved", "u@e.com"),
		"PerfIssueDetail":      PerfIssueDetail(PerfIssueDetailData{Issue: trace.PerfIssue{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Culprit: "db", Status: "unresolved", Count: 12, SampleTraceID: "t1"}, Evidence: PerfEvidence{Count: 12, TotalUS: 1000, MaxUS: 300, ParentOp: "http", SequentialPct: 80, MaxConcurrency: 1, URLs: []string{"/a"}, HasTotal: true, HasMax: true, HasSequential: true}, Query: "SELECT * FROM line_items WHERE order_id = $1", QueryOp: "db.sql.query", SpanDurationUS: 6000, DBSystem: "postgresql", Code: &PerfCodeLoc{File: "app/reports/orders.py", Line: "88", Function: "build_report_rows"}}, "u@e.com"),
		"IncidentsList":        IncidentsList(7, []IncidentRow{{Incident: uptime.Incident{ID: 1, StartedAt: now.Add(-time.Hour), Cause: "t"}, MonitorName: "web"}, {Incident: uptime.Incident{ID: 2, StartedAt: now.Add(-5 * time.Hour), ResolvedAt: &now, Cause: "5xx"}, MonitorName: "api"}}, 2, 120, "u@e.com"),
		"RegressionsList":      RegressionsList(7, []trace.Regression{{ID: 1, TargetKind: "endpoint_p95", Target: "GET /", Metric: "duration", Status: "open", BaselineValue: 100, PeakValue: 300, StartedAt: now}, {ID: 2, TargetKind: "webvital_p75", Target: "/home", Metric: "lcp", Status: "resolved", BaselineValue: 2000, PeakValue: 4000, StartedAt: now.Add(-2 * time.Hour), ResolvedAt: &now}}, []string{"после деплоя v1.2.3 (5 минут назад)", ""}, "open", "u@e.com", false, true, map[int64]string{1: "u@e.com"}),
		"Probes":               Probes(o, []ProbeRow{{Probe: uptime.Probe{ID: 1, Region: "eu", Name: "eu-1", LastSeenAt: &now}, Status: "online"}, {Probe: uptime.Probe{ID: 2, Region: "us", Name: "us-1"}, Status: "offline"}}, "tok", "run", "", "u@e.com"),
		"ProjectSettings":      ProjectSettings(org.Project{ID: 7, OrgID: 1, Slug: "web", Name: "Web", Platform: "go"}, []ProjectKeyView{{Key: org.Key{ID: 1, PublicKey: "pk", Kind: org.KindServer, Revoked: false}, DSN: "https://pk@dsn"}, {Key: org.Key{ID: 2, PublicKey: "old", Kind: org.KindLegacy, Revoked: true}, DSN: "https://old@dsn"}}, "", "u@e.com", PerfSettingsForm{SampleRate: "1", ApdexMS: "500", NPlusOneMin: "5", SlowDBMs: "300"}, RegressionSettingsForm{ThresholdPct: "20", RecoveryPct: "10", WindowMinutes: "60", MinSamples: "100", Enabled: true}, 30, []DeprecatedPathView{{Path: "/logs", LastSeenAt: now, Hits: 3}}),
		"ProjectsList":         ProjectsList([]ProjectListItem{{Project: org.Project{ID: 1, Name: "web", Slug: "web", Platform: "go"}, CanManage: true}, {Project: org.Project{ID: 2, Name: "api", Slug: "api", Platform: "php"}, CanManage: false}}, []OrgOption{{ID: 1, Name: "Acme"}}, nil, "", "u@e.com"),
		"OrgProjects":          OrgProjects(o, []org.Project{{ID: 1, OrgID: 1, Name: "web", Slug: "web", Platform: "go"}, {ID: 2, OrgID: 1, Name: "api", Slug: "api", Platform: "php"}}, []OrgOption{{ID: 1, Name: "Acme"}}, nil, "", "u@e.com"),
		"Onboarding":           Onboarding("err", "acme", "Acme", "web", "Web", "go", "u@e.com"),
		"AlertDeliveries":      AlertDeliveries(7, []notify.FailedJob{{ID: 1, ChannelKind: "email", Target: "a@b.c", LastError: strings.Repeat("x", 400), Attempts: 5, CreatedAt: now}}, true, "u@e.com"),
		"Profile":              Profile("u@e.com", "", "ok", true, []LinkedIdentity{{Provider: "yandex", DisplayName: "Я", Email: "u@ya.ru", CanUnlink: true}}, []LinkableProvider{{Name: "github", DisplayName: "GitHub"}}, false, "u@e.com"),
		"Maintenance":          Maintenance(7, []uptime.Window{{ID: 1, Name: "one", StartsAt: &now, EndsAt: &now, Timezone: "UTC"}, {ID: 2, Name: "wk", Weekly: true, Weekday: 1, StartTime: "02:00", EndTime: "04:00", Timezone: "Europe/Moscow"}}, nil, "err", "u@e.com"),
		"ProjectSetup":         ProjectSetup(org.Project{ID: 7, Slug: "web", Name: "Web", Platform: "go"}, "https://dsn", []SetupSnippet{{Lang: "Go", Install: "go get x", Code: "code"}}, "", "u@e.com"),
		"StatusPagesSet":       StatusPagesSettings(7, "https://g.example", []StatusPageForm{{ID: 1, Title: "T", Description: "d", Enabled: true, Monitors: []StatusPageFormMonitor{{ID: 10, MonitorName: "web", Selected: true, DisplayName: "W"}, {ID: 20, MonitorName: "api"}}}}, StatusPageForm{Monitors: []StatusPageFormMonitor{{ID: 10, MonitorName: "web"}}}, true, "err", "u@e.com"),
		"PublicStatus":         PublicStatusPage(StatusPageView{Title: "S", Description: "d", Overall: "partial", Monitors: []StatusMonitorView{{Name: "web", Status: "up", Uptime90d: uptime.UptimeStat{Total: 100, OK: 99}, Bars: stubC}, {Name: "api", Status: "down", Bars: stubC}}, Incidents: []StatusIncidentView{{Name: "I", StartedAt: "t", Ongoing: true}, {Name: "J", StartedAt: "t", Duration: 2 * time.Hour}}, Maintenance: []StatusWindowView{{Name: "M", From: "a", To: "b"}}}),
		"ProfileFlame":         ProfileFlame(ProfileFlameVM{ProjectID: 7, Service: "web", Type: "cpu", Transaction: "GET /", Environment: "production", Range: TimeRangeVM{Key: "24h"}, Chart: stubC}, "u@e.com"),
		"TraceWaterfall":       TraceWaterfall(TraceWaterfallData{ProjectID: 7, TraceID: "tr", Transaction: "GET /", TotalUS: 1000, Timestamp: now, Waterfall: stubC, ShownRows: 5, TotalRows: 10, HasProfile: true, From: "endpoint", FromTransaction: "GET /"}, "u@e.com"),
		"TraceFlame":           TraceFlame(TraceFlameData{TraceID: "tr", Chart: stubC}, "u@e.com"),
		"DocsIndex":            DocsIndex([]DocsGroup{{Key: "docs.group.getting_started", Pages: []docs.Page{{Slug: "q", Group: "docs.group.getting_started", Title: "Q"}}}}, "u@e.com"),
		"DocsPage":             DocsPage("q", "Q", "<p>body</p>", []docs.Page{{Slug: "q", Title: "Q"}}, "u@e.com"),
		"ConfirmPage":          ConfirmPage("T", "M", "OK", "/back", "/do", []HiddenField{{Name: "id", Value: "1"}}, "u@e.com"),
		"ErrorPage":            ErrorPage(404, "нет", "u@e.com"),
		"Login":                Login("err", "", "u@e.com", []OAuthButton{{Name: "yandex", Label: "Я"}}),
		"RegisterForm":         RegisterForm("", false, "", "", []OAuthButton{{Name: "github", Label: "GH"}}),
		"RegisterStub":         RegisterStub("", "closed", "", nil),
		"SSOLogin":             SSOLogin("err"),
		"InviteAccept":         InviteAccept("tok", "err", "u@e.com", org.InviteInfo{}),
		"ForgotPassword":       ForgotPassword("err", false, true, "u@e.com"),
		"ResetPassword":        ResetPassword("tok", "err", true),

		"About":              About(version.Info{Version: "v0.22.1", Commit: "abcdef1", Date: "2026-08-01", Go: "go1.26", Stamped: true}, "u@e.com"),
		"AlertSuppression":   AlertSuppression(7, []SuppressionEdgeView{{ID: 1, ParentLabel: "хост: web-1", ChildLabel: "монитор: api"}}, []SuppressionNodeOption{{ID: 1, Name: "web-1"}}, []SuppressionNodeOption{{ID: 2, Name: "api"}}, []SuppressionPreviewView{{ParentLabel: "web-1", Children: []string{"api"}}}, 5*time.Minute, nil, "", "u@e.com"),
		"DependenciesScreen": DependenciesScreen(7, []DependencyRow{{Kind: "postgres", Target: "db", Calls: 100, P50US: 1000, P95US: 5000, ErrorRate: 0.01}}, DepsFilter{Range: TimeRangeVM{Key: "24h"}, Active: true}, stubC, false, false, "u@e.com"),
		"DeploymentsScreen":  DeploymentsScreen(7, []DeploymentRow{{Version: "v1.2.3", Environment: "production", URL: "https://ci/1", LinkURL: "https://ci/1", IsLink: true, Changelog: "fix bug", DeployedAt: now}}, "u@e.com"),
		"Escalations":        Escalations(7, []alert.Channel{{ID: 1, Kind: "email", Enabled: true, Target: "a@b.c"}}, EscalationLadderForm{Severity: "critical", Steps: []EscalationStepForm{{StepNo: 1, DelayMinutes: "5", Selected: map[int64]bool{1: true}}}}, EscalationLadderForm{Severity: "warning"}, map[string]escalation.Ladder{}, "", "", "u@e.com"),
		"Exports":            Exports(7, []ExportView{{ID: 1, KindLabel: "issues", FormatLabel: "csv", Status: "done", Rows: 100, Size: 2048, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), Author: "u@e.com", CanDownload: true, CanDelete: true}}, true, "u@e.com", true, "", nil),
		"HostDetail":         HostDetail(HostDetailVM{ProjectID: 7, Host: host.Host{ID: 1, ProjectID: 7, Name: "web-1", FirstSeen: now, LastSeen: now, Environment: "production", Role: "web"}, Range: TimeRangeVM{Key: "24h"}, StatusKind: "ok", CanOperate: true, ServerVersion: "v0.22.1"}, "u@e.com"),
		"HostSettings":       HostSettings(7, host.DefaultSettings(), "curl https://x/install.sh | sh", "cfg", "", nil, "", HostGroupThresholdsVM{}, "u@e.com"),
		"HostsList":          HostsList(7, []HostRowVM{{Name: "web-1", StatusKind: "ok", CPU: &cpu04, LastSeen: now}}, false, 100, HostsFilterVM{}, HostsFacets{}, nil, "curl https://x/install.sh | sh", "cfg", "", "u@e.com", false),
		"Overview":           Overview(7, "24h", []GroupCard{NewGroupCard(group, []incidentgroup.FeedItem{{Source: "host", IncidentID: 1, Title: "disk", StartedAt: now, Severity: "critical"}})}, []incidentgroup.FeedItem{{Source: "uptime", IncidentID: 2, Title: "down", StartedAt: now}}, nil, nil, FeedCaps{OpenGroups: 20, OutOfGroup: 20, ClosedGroups: 10, ClosedItems: 10}, true, StatusLine{Uptime: uptime.UptimeStat{Total: 100, OK: 99}, HostsOverThreshold: 1, NewIssues24h: 3}, []DeploymentRow{{Version: "v1.2.3", Environment: "production", DeployedAt: now}}, "u@e.com"),
		"LogsScreen":         LogsScreen(7, []LogRow{NewLogRow(log.LogRow{Timestamp: now, Severity: "ERROR", Body: "boom", Service: "web", Environment: "production"})}, LogsFilter{Range: TimeRangeVM{Key: "24h"}}, false, "", LogsHistogram{Empty: true}, LogFacets{}, "u@e.com", LogSavedFiltersPanel{}, ""),
		"NoProjects":         NoProjects(true, "u@e.com"),
		"RecipesList":        RecipesList(7, []RecipeCardVM{{ID: recipe.ID, DataArrives: true, CreatedRules: 1, TotalRules: 3}}, "u@e.com"),
		"RecipeDetail":       RecipeDetail(RecipeDetailVM{ProjectID: 7, Recipe: recipe, DataArrives: false, CanOperate: true}, "u@e.com"),
		"SLODetailScreen":    SLODetailScreen(SLODetailVM{ProjectID: 7, ID: 1, Name: "availability", Kind: "availability", TargetPct: 99.9, WindowDays: 30, HasData: true, AttainmentPct: 99.95, BudgetRemainingPct: 50, Status: "healthy", Chart: stubC}, "u@e.com"),
		"SLOsScreen":         SLOsScreen(7, []SLORow{{ID: 1, Name: "availability", Kind: "availability", TargetPct: 99.9, AttainmentPct: 99.95, BudgetRemainingPct: 50, HasData: true, Status: "healthy"}}, []SLOMonitorOption{{ID: 1, Name: "web"}}, nil, "", "u@e.com"),
		"TraceExpired":       TraceExpired(TraceExpiredData{ProjectID: 7, TraceID: "tr1", RetentionDays: 90, From: "endpoint", FromTransaction: "GET /"}, "u@e.com"),
		"TraceUnavailable":   TraceUnavailable(TraceExpiredData{ProjectID: 7, TraceID: "tr1", From: "endpoint", FromTransaction: "GET /"}, "u@e.com"),
	}
	return m
}

// ошибка проверяется только при fw.tripped: вывод части шаблонов зависит от
// time.Now(), и на хвостовых смещениях writer иногда не добирает свой лимит.
func TestRenderPropagatesWriteErrors(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	trippedAny := false
	for name, comp := range pageComponents() {
		var good strings.Builder
		if err := comp.Render(ctx, &good); err != nil {
			t.Fatalf("%s: базовый рендер упал: %v", name, err)
		}
		full := good.Len()
		for _, k := range writeOffsets(full) {
			fw := &failAfter{n: k}
			err := comp.Render(ctx, fw)
			if fw.tripped {
				trippedAny = true
				if err == nil {
					t.Fatalf("%s: обрыв записи на %d/%d байт проглочен (ошибка не всплыла)", name, k, full)
				}
			}
		}
	}
	if !trippedAny {
		t.Fatal("ни один обрыв записи не сработал — тест ничего не проверил")
	}
}

// проверка каждого байта вывода растёт квадратично от размера шаблона; вместо
// этого плотно берём начало и дальше — равномерную выборку по линии.
func writeOffsets(full int) []int {
	const (
		dense   = 96  // первые N байт — подряд
		samples = 160 // сколько точек дальше
	)
	if full <= dense+samples {
		out := make([]int, full)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, 0, dense+samples)
	for i := 0; i < dense; i++ {
		out = append(out, i)
	}
	step := (full - dense) / samples
	if step < 1 {
		step = 1
	}
	for k := dense; k < full; k += step {
		out = append(out, k)
	}
	return out
}

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
