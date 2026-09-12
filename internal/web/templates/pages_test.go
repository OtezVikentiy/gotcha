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

func renderTo(t *testing.T, c templ.Component) string {
	t.Helper()
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	var sb strings.Builder
	if err := c.Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// nil уронил бы рендер — везде, где шаблон рендерит переданный templ.Component.
func stub() templ.Component { return templ.Raw("<svg data-stub></svg>") }

func ptrTime(t time.Time) *time.Time { return &t }
func ptrInt64(v int64) *int64        { return &v }

func TestIssuesListPopulatedVsEmpty(t *testing.T) {
	rows := []IssueRow{
		{Issue: issue.Issue{ID: 1, Title: "boom error", Level: "error", Status: "unresolved", TimesSeen: 42, LastSeen: time.Now().Add(-time.Hour)}, Sparkline: stub()},
		{Issue: issue.Issue{ID: 2, Title: "minor warn", Level: "warning", Status: "resolved", TimesSeen: 3, LastSeen: time.Now().Add(-24 * time.Hour), AssigneeEmail: "dev@x.io"}, Sparkline: stub()},
	}
	gs := GettingStartedVM{ProjectID: 7, OrgID: 1, Done: 3, Step2Done: true}
	out := renderTo(t, IssuesList(7, rows, IssuesFilter{Status: "unresolved"}, 1, 2, "u@e.com", []string{"production", "staging"}, &QuotaBanner{Text: "почти лимит", Href: "/x"}, gs, true, true, false))
	if !strings.Contains(out, "boom error") || !strings.Contains(out, "badge-danger") {
		t.Error("список issue должен содержать заголовок и бейдж уровня")
	}
	if !strings.Contains(out, "почти лимит") {
		t.Error("баннер квоты должен отрендериться")
	}

	if !strings.Contains(out, `class="card-toolbar"`) {
		t.Error("непустой список должен рисовать тулбар .card-toolbar над таблицей")
	}
	if n := strings.Count(out, `<details class="dropdown-control"`); n != 2 {
		t.Errorf("тулбар при canOperate=true должен нести ровно 2 раскрывающиеся кнопки экспорта, got %d", n)
	}

	noExport := renderTo(t, IssuesList(7, rows, IssuesFilter{}, 1, 2, "u@e.com", nil, nil, gs, false, false, false))
	if !strings.Contains(noExport, `class="card-toolbar"`) {
		t.Error("непустой список при canOperate=false должен рисовать тулбар с массовыми действиями")
	}
	if strings.Contains(noExport, "dropdown-control") {
		t.Error("при canOperate=false в тулбаре не должно быть кнопок экспорта")
	}

	empty := renderTo(t, IssuesList(7, nil, IssuesFilter{}, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, false, false, false))
	if strings.Contains(empty, "boom error") {
		t.Error("пустой список не должен содержать строк")
	}
	emptyOp := renderTo(t, IssuesList(7, nil, IssuesFilter{}, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, true, true, false))
	for name, body := range map[string]string{"canOperate=false": empty, "canOperate=true": emptyOp} {
		for _, marker := range []string{"card-toolbar", "issues-bulk", "dropdown-control"} {
			if strings.Contains(body, marker) {
				t.Errorf("пустой список (%s) не должен содержать %q", name, marker)
			}
		}
	}
}

// поиск первым — самое частое действие; сортировка — не фильтр, уходит в конец перед «Применить».
func TestIssuesListFilterOrder(t *testing.T) {
	out := renderTo(t, IssuesList(7, nil, IssuesFilter{Range: TimeRangeVM{Key: "24h"}}, 1, 0, "u@e.com", []string{"prod"}, nil, GettingStartedVM{}, false, false, false))
	start := strings.Index(out, `class="issues-filters"`)
	if start < 0 {
		t.Fatal("нет формы фильтров .issues-filters")
	}
	form := out[start:]
	if end := strings.Index(form, "</form>"); end >= 0 {
		form = form[:end]
	}
	order := []string{`name="q"`, `name="status"`, `name="level"`, `name="env"`, `name="period"`, `name="sort"`, `type="submit"`}
	prev := -1
	for _, field := range order {
		idx := strings.Index(form, field)
		if idx < 0 {
			t.Fatalf("в форме фильтров нет поля %s", field)
		}
		if idx <= prev {
			t.Errorf("поле %s стоит раньше предыдущего по порядку %v", field, order)
		}
		prev = idx
	}
}

// гейт карточки — CanOperate, не CanManage: 2 из 5 шагов уже давно открыты оператору.
// CanManage остаётся только у шага 4a («Позвать команду», owner/admin-only).
func TestGettingStartedChecklistGatedByCanOperate(t *testing.T) {
	opGS := GettingStartedVM{ProjectID: 7, OrgID: 9, Done: 1, CanOperate: true, CanManage: false}
	out := renderTo(t, IssuesList(7, nil, IssuesFilter{}, 1, 0, "u@e.com", nil, nil, opGS, false, false, false))
	if !strings.Contains(out, `class="card getting-started"`) {
		t.Fatal("оператор (CanOperate=true) должен видеть чек-лист «Первые шаги»")
	}
	for _, href := range []string{`href="/projects/7/setup"`, `href="/projects/7/alerts"`, `href="/projects/7/monitors"`} {
		if !strings.Contains(out, href) {
			t.Errorf("оператору должна быть видна рабочая ссылка %q", href)
		}
	}
	if strings.Contains(out, `href="/orgs/9/settings"`) {
		t.Error("оператору без CanManage не должно рендериться рабочей ссылки на настройки организации (шаг 4a) — это мёртвый контрол (403)")
	}
	if !strings.Contains(out, "gs-todo-locked") {
		t.Error("незакрытый шаг 4a у оператора без CanManage должен рендериться неактивным (gs-todo-locked), а не пропадать бесследно")
	}

	// CanManage без CanOperate — гипотетическая комбинация, но проверяет именно гейт карточки.
	adminOnlyGS := GettingStartedVM{ProjectID: 7, OrgID: 9, Done: 1, CanOperate: false, CanManage: true}
	out2 := renderTo(t, IssuesList(7, nil, IssuesFilter{}, 1, 0, "u@e.com", nil, nil, adminOnlyGS, false, false, false))
	if strings.Contains(out2, `class="card getting-started"`) {
		t.Error("CanManage без CanOperate не должен показывать чек-лист — гейт карточки теперь CanOperate, а не CanManage")
	}
}

// врезка «отказы по ключу» рисуется ровно в одном месте — чек-лист либо пустое состояние, не оба сразу.
// сценарии идут через VM напрямую: нужная пара условий недостижима через настоящий HTTP-запрос.
func TestKeyRejectsBodyRendersOnceAcrossChecklistAndEmptyState(t *testing.T) {
	rejects := []KeyRejectView{{Kind: "key_invalid", Hits: 5, LastSeenAt: time.Now()}}
	wantReason := i18n.Tf(context.Background(), "ingest_signals.rejects.key_invalid", "hits", "5")

	visible := GettingStartedVM{ProjectID: 7, OrgID: 9, Done: 1, CanOperate: true, KeyRejects: rejects}
	out := renderTo(t, IssuesList(7, nil, IssuesFilter{}, 1, 0, "u@e.com", nil, nil, visible, false, false, false))
	section := sectionBetween(t, out, `<section class="card getting-started">`, "</section>")
	if !strings.Contains(section, wantReason) {
		t.Errorf("видимый чек-лист должен нести причину отказа %q внутри своей секции: %s", wantReason, section)
	}
	if strings.Contains(out, `class="notice notice--warn"`) {
		t.Error("чек-лист виден — пустое состояние не должно дублировать врезку notice--warn")
	}

	hidden := GettingStartedVM{ProjectID: 7, OrgID: 9, Done: 1, CanOperate: false, KeyRejects: rejects}
	out2 := renderTo(t, IssuesList(7, nil, IssuesFilter{}, 1, 0, "u@e.com", nil, nil, hidden, false, false, false))
	if strings.Contains(out2, `class="card getting-started"`) {
		t.Fatal("CanOperate=false не должен показывать чек-лист")
	}
	notice := sectionBetween(t, out2, `<div class="notice notice--warn">`, "</div>")
	if !strings.Contains(notice, wantReason) {
		t.Errorf("чек-лист скрыт — пустое состояние должно нести причину отказа %q: %s", wantReason, notice)
	}
}

// ищет текст внутри конкретной секции, не по всему телу — иначе поломка одной врезки маскируется другой.
func sectionBetween(t *testing.T, s, openTag, closeTag string) string {
	t.Helper()
	i := strings.Index(s, openTag)
	if i < 0 {
		t.Fatalf("маркер %q не найден: %s", openTag, s)
	}
	rest := s[i+len(openTag):]
	j := strings.Index(rest, closeTag)
	if j < 0 {
		t.Fatalf("закрывающий маркер %q не найден после %q: %s", closeTag, openTag, s)
	}
	return rest[:j]
}

func TestIssueDetail(t *testing.T) {
	it := issue.Issue{ID: 5, Title: "NPE", Level: "error", Status: "unresolved", Culprit: "svc.Do", TimesSeen: 7, FirstSeen: time.Now().Add(-48 * time.Hour), LastSeen: time.Now(), AssigneeID: ptrInt64(2), AssigneeEmail: "dev@x.io"}
	members := []org.Member{{UserID: 2, Email: "dev@x.io", Role: org.RoleAdmin}}
	ev := event.Stored{ID: "ev1", Level: "error", ExceptionType: "NPE", ExceptionValue: "nil ptr", Environment: "production", Release: "1.2.3", TraceID: "abc", Tags: map[string]string{"k": "v"}}
	frames := []Frame{{Function: "main", Module: "app", Filename: "main.go", Lineno: 10, InApp: true}}
	out := renderTo(t, IssueDetail(it, members, stub(), TimeRangeVM{Key: "24h"}, []event.Stored{ev}, "ev1", &ev, frames, "u@e.com", true, true, "", "", true, true, false))
	if !strings.Contains(out, "NPE") || !strings.Contains(out, "main.go:10") {
		t.Error("деталь issue должна показать исключение и локацию кадра")
	}
	if !strings.Contains(out, "dev@x.io") {
		t.Error("должен отрисоваться назначенный")
	}
}

func TestPerformanceList(t *testing.T) {
	rows := []EndpointRow{
		{Stat: trace.EndpointStat{Transaction: "GET /api", Count: 1000, Throughput: 12, P50: 5000, P95: 20000, P99: 50000, FailureRate: 0.02, ApdexScore: 0.95, Environments: []string{"production"}}, Sparkline: stub()},
	}
	out := renderTo(t, PerformanceList(7, rows, 1, PerfFilter{Range: TimeRangeVM{Key: "24h"}, Sort: "throughput"}, []string{"production"}, 500,
		// без примеров человек не догадается, что в имя транзакции попал идентификатор.
		[]CardinalityNotice{{Field: "transaction name", Limit: 10000, Collapsed: 47213,
			Samples: []string{"GET /users/8812/profile", "GET /users/8813/profile"}}}, "u@e.com", false, false))
	if !strings.Contains(out, "GET /api") {
		t.Error("список должен содержать транзакцию")
	}
	if !strings.Contains(out, "GET /users/8812/profile") {
		t.Error("страница производительности должна показывать примеры схлопнутых имён")
	}
	if !strings.Contains(out, "/docs/cardinality") {
		t.Error("предупреждение должно вести на страницу документации")
	}

	capped := renderTo(t, PerformanceList(7, rows, 1, PerfFilter{Range: TimeRangeVM{Key: "24h"}}, []string{"production"}, 500, nil, "u@e.com", false, true))
	if !strings.Contains(capped, "больше 20 000 разных эндпойнтов") {
		t.Error("при capped=true должно показываться предупреждение об усечении окна CH-запросом")
	}

	empty := renderTo(t, PerformanceList(7, nil, 0, PerfFilter{}, nil, 0, nil, "u@e.com", false, false))
	if strings.Contains(empty, "GET /api") {
		t.Error("пустой список не должен содержать транзакций")
	}
}

func TestEndpointDetail(t *testing.T) {
	d := EndpointDetailData{
		ProjectID: 7, Transaction: "GET /api", Range: TimeRangeVM{Key: "24h"}, Environment: "production", ApdexT: 500,
		LatencyChart: stub(), Throughput: stub(), Histogram: stub(), StepLabel: "1h",
		Slowest: []SlowestTraceRow{
			{Row: trace.TraceRow{TraceID: "t1", DurationUS: 120000, Timestamp: time.Now(), Status: "ok"}},
			{Row: trace.TraceRow{TraceID: "t2-expired", DurationUS: 90000, Timestamp: time.Now().Add(-60 * 24 * time.Hour), Status: "ok"}, Expired: true},
		},
		PerfIssues: []trace.PerfIssue{{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Status: "unresolved", Count: 9}},
		Vitals:     []VitalPanelRow{{Vital: trace.Vital{Name: "lcp", P75: 2400, Rating: "good", Count: 50}, Chart: stub()}},
	}
	out := renderTo(t, EndpointDetail(d, "u@e.com"))
	if !strings.Contains(out, "GET /api") || !strings.Contains(out, "N+1") {
		t.Error("деталь эндпойнта должна показать транзакцию и perf-issue")
	}
	if !strings.Contains(out, `<a href="/traces/t1?from=endpoint`) {
		t.Error("не истёкший трейс должен рендериться кликабельной ссылкой")
	}
	if strings.Contains(out, `<a href="/traces/t2-expired`) {
		t.Error("истёкший трейс не должен рендериться ссылкой (спанов уже нет — вела бы на 404)")
	}
	if !strings.Contains(out, "t2-expired") {
		t.Error("истёкший трейс всё равно должен показывать trace_id текстом")
	}
	// «Порог Apdex» (мс) — свой ключ apdex_threshold, не apdex (это подсказка индекса 0..1).
	if !strings.Contains(out, i18n.T(ruCtx(), "perf.help.apdex_threshold")) {
		t.Error("подсказка «Порог Apdex» должна использовать perf.help.apdex_threshold")
	}
	if strings.Contains(out, i18n.T(ruCtx(), "perf.help.apdex")) {
		t.Error("подсказка «Порог Apdex» не должна использовать perf.help.apdex (это подсказка индекса 0..1)")
	}
}

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
	out := renderTo(t, MonitorDetail(m, "up", stat, stat, stat, stub(), TimeRangeVM{Key: "24h"}, checks, incidents, 1, int64(len(incidents)), true, true, "https://gotcha.example", "u@e.com", false))
	if !strings.Contains(out, "api") || !strings.Contains(out, "badge-good") || !strings.Contains(out, "badge-danger") {
		t.Error("деталь монитора должна показать имя и статусы проверок")
	}

	noManage := renderTo(t, MonitorDetail(m, "down", stat, stat, stat, stub(), TimeRangeVM{Key: "24h"}, nil, nil, 1, 0, false, false, "https://x", "u@e.com", false))
	if !strings.Contains(noManage, "api") {
		t.Error("монитор без прав всё равно рендерится")
	}
}

func TestMonitorsList(t *testing.T) {
	last := time.Now().Add(-2 * time.Minute)
	rows := []MonitorRow{
		{Monitor: uptime.Monitor{ID: 1, Name: "web", Kind: uptime.KindHTTP}, Status: "up", Uptime24h: uptime.UptimeStat{Total: 10, OK: 10}, AvgLatencyMs: 80, Bars: stub(), LastChecked: &last},
		{Monitor: uptime.Monitor{ID: 2, Name: "db", Kind: uptime.KindTCP}, Status: "down", Bars: stub()},
	}
	out := renderTo(t, MonitorsList(7, rows, true, "u@e.com", false))
	if !strings.Contains(out, "web") || !strings.Contains(out, "db") {
		t.Error("список мониторов должен содержать имена")
	}
	empty := renderTo(t, MonitorsList(7, nil, false, "u@e.com", false))
	if strings.Contains(empty, ">web<") {
		t.Error("пустой список не содержит мониторов")
	}
}

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

func TestAlerts(t *testing.T) {
	rules := []alert.Rule{
		{ID: 1, Kind: alert.KindNewIssue, Enabled: true},
		{ID: 2, Kind: alert.KindSpike, Enabled: false, Threshold: 100, WindowMinutes: 5},
	}
	channels := []alert.Channel{
		{ID: 1, Kind: "email", Enabled: true, Target: "team@x.io"},
		{ID: 2, Kind: "webhook", Enabled: false, Target: "https://hook"},
	}
	out := renderTo(t, Alerts(7, rules, channels, true, true, false, nil, "", "u@e.com"))
	if !strings.Contains(out, "team@x.io") || !strings.Contains(out, "https://hook") {
		t.Error("каналы должны отрендериться")
	}
	outErr := renderTo(t, Alerts(7, nil, nil, false, true, false, nil, "ошибка сохранения", "u@e.com"))
	if !strings.Contains(outErr, "ошибка сохранения") {
		t.Error("ошибка должна отрендериться")
	}
}

func TestAlertsSecretKeyInsecureWarning(t *testing.T) {
	channels := []alert.Channel{{ID: 1, Kind: "telegram", Enabled: true, Target: "@ch"}}
	warnText := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "secret.insecure_warning")

	insecure := renderTo(t, Alerts(7, nil, channels, true, true, true, nil, "", "u@e.com"))
	if !strings.Contains(insecure, warnText) {
		t.Error("на dev-ключе предупреждение о секретах отсутствует")
	}

	secure := renderTo(t, Alerts(7, nil, channels, true, true, false, nil, "", "u@e.com"))
	if strings.Contains(secure, warnText) {
		t.Error("на сильном ключе предупреждение о секретах не должно рендериться")
	}
}

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
	out := renderTo(t, OrgSettings(o, members, 1, quotas, true, "", "https://gotcha/invite/tok", sso, "owner@x.io", nil, SubjectPurgeVM{}, nil, nil))
	if !strings.Contains(out, "owner@x.io") || !strings.Contains(out, "admin@x.io") {
		t.Error("участники должны отрендериться")
	}
	if !strings.Contains(out, "https://gotcha/invite/tok") {
		t.Error("пригласительная ссылка должна отрендериться")
	}
	out2 := renderTo(t, OrgSettings(o, members, 2, quotas, false, "боом", "", SSOSettings{}, "admin@x.io", &QuotaBanner{Text: "лимит", Href: "/x"}, SubjectPurgeVM{}, nil, nil))
	if !strings.Contains(out2, "боом") {
		t.Error("ошибка орга должна отрендериться")
	}
}

func TestOrgSettingsSecretKeyInsecureWarning(t *testing.T) {
	o := org.Org{ID: 1, Slug: "acme", Name: "Acme"}
	members := []org.Member{{UserID: 1, Email: "owner@x.io", Role: org.RoleOwner}}
	warnText := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "secret.insecure_warning")

	insecureSSO := SSOSettings{CanConfigure: true, SecretKeyInsecure: true}
	insecure := renderTo(t, OrgSettings(o, members, 1, nil, true, "", "", insecureSSO, "owner@x.io", nil, SubjectPurgeVM{}, nil, nil))
	if !strings.Contains(insecure, warnText) {
		t.Error("на dev-ключе предупреждение о client_secret отсутствует")
	}

	secureSSO := SSOSettings{CanConfigure: true, SecretKeyInsecure: false}
	secure := renderTo(t, OrgSettings(o, members, 1, nil, true, "", "", secureSSO, "owner@x.io", nil, SubjectPurgeVM{}, nil, nil))
	if strings.Contains(secure, warnText) {
		t.Error("на сильном ключе предупреждение о client_secret не должно рендериться")
	}
}

func TestTeams(t *testing.T) {
	o := org.Org{ID: 1, Slug: "acme", Name: "Acme"}
	orgMembers := []org.Member{{UserID: 1, Email: "a@x.io", Role: org.RoleOwner}, {UserID: 2, Email: "b@x.io", Role: org.RoleMember}}
	orgProjects := []org.Project{{ID: 10, Name: "web"}, {ID: 20, Name: "api"}}
	teams := []TeamView{
		{Team: org.Team{ID: 100, Slug: "core", Name: "Core"}, Members: []org.Member{{UserID: 1, Email: "a@x.io", Role: org.RoleOwner}}, Projects: []org.Project{{ID: 10, Name: "web"}}},
	}
	out := renderTo(t, Teams(o, teams, orgMembers, orgProjects, nil, "", "u@e.com"))
	if !strings.Contains(out, "Core") || !strings.Contains(out, "web") {
		t.Error("команды и проекты должны отрендериться")
	}
	outEmpty := renderTo(t, Teams(o, nil, orgMembers, orgProjects, nil, "ошибка", "u@e.com"))
	if !strings.Contains(outEmpty, "ошибка") {
		t.Error("ошибка должна отрендериться")
	}
}

func TestProfilesList(t *testing.T) {
	services := []profile.ServiceInfo{
		{Service: "web", Type: "cpu", Transaction: "GET /", Weight: 2_000_000_000, Unit: "nanoseconds", Samples: 1000, Environments: []string{"production"}},
		{Service: "web", Type: "alloc_space", Transaction: "POST /", Weight: 5 * 1024 * 1024, Unit: "bytes", Samples: 500},
	}
	out := renderTo(t, ProfilesList(7, services, TimeRangeVM{Key: "24h"}, "production", "u@e.com", false))
	if !strings.Contains(out, "web") {
		t.Error("список профилей должен содержать сервис")
	}
	empty := renderTo(t, ProfilesList(7, nil, TimeRangeVM{Key: "24h"}, "", "u@e.com", false))
	if strings.Contains(empty, ">GET /<") {
		t.Error("пустой список профилей")
	}
}

func TestProfileRegressionsList(t *testing.T) {
	now := time.Now()
	regs := []profile.Regression{
		{ID: 1, Service: "web", ProfileType: "cpu", Function: "hot()", Status: "open", BaselineShare: 0.1, PeakShare: 0.3, CurrentShare: 0.25, StartedAt: now.Add(-time.Hour)},
		{ID: 2, Service: "api", ProfileType: "heap", Function: "leak()", Status: "resolved", BaselineShare: 0.05, PeakShare: 0.2, StartedAt: now.Add(-3 * time.Hour), ResolvedAt: ptrTime(now.Add(-time.Hour))},
	}
	out := renderTo(t, ProfileRegressionsList(7, regs, "open", "u@e.com", true))
	if !strings.Contains(out, "hot()") {
		t.Error("регрессии профилей должны содержать функцию")
	}
}

func TestMetricsListAndDetail(t *testing.T) {
	metrics := []metric.MetricInfo{{Name: "http.rps", Type: "gauge", Unit: "1/s"}, {Name: "queue.depth", Type: "histogram", Unit: ""}}
	out := renderTo(t, MetricsList(7, metrics, "production", "u@e.com", false, 0, false))
	if !strings.Contains(out, "http.rps") {
		t.Error("список метрик должен содержать имя")
	}

	vm := MetricDetailVM{
		ProjectID: 7, Info: metric.MetricInfo{Name: "http.rps", Type: "histogram", Unit: "ms"},
		Range: TimeRangeVM{Key: "24h"}, Agg: "avg", Environment: "production", Environments: []string{"production", "staging"},
		Labels: map[string][]string{"route": {"/a", "/b"}}, LabelKey: "route", LabelValue: "/a",
		Chart: stub(), Percentiles: true,
	}
	outD := renderTo(t, MetricDetail(vm, "u@e.com"))
	if !strings.Contains(outD, "http.rps") {
		t.Error("деталь метрики должна содержать имя")
	}
}

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
	// datalist с известными именами — опечатка в свободном поле создавала бы правило, которое не сработает.
	known := []string{"http.rps", "process.memory.usage"}
	out := renderTo(t, MetricAlerts(7, rules, incidents, known, nil, "", "u@e.com"))
	if !strings.Contains(out, "process.memory.usage") {
		t.Error("форма правила должна подсказывать уже приходившие метрики")
	}
	if !strings.Contains(out, "http.rps") || !strings.Contains(out, "err.rate") {
		t.Error("правила метрик должны отрендериться")
	}
}

func TestWebVitalsList(t *testing.T) {
	pages := []trace.PageVitals{
		{Transaction: "/home", LCP: trace.Vital{Name: "lcp", P75: 2400, Rating: "good"}, INP: trace.Vital{Name: "inp", P75: 300, Rating: "needs-improvement"}, CLS: trace.Vital{Name: "cls", P75: 0.3, Rating: "poor"}, Count: 500, Environments: []string{"production"}},
	}
	out := renderTo(t, WebVitalsList(7, pages, PerfFilter{Range: TimeRangeVM{Key: "24h"}}, []string{"production"}, "u@e.com", false))
	if !strings.Contains(out, "/home") || !strings.Contains(out, "badge-good") || !strings.Contains(out, "badge-danger") {
		t.Error("список web-vitals должен содержать страницу и разные бейджи рейтинга")
	}
	empty := renderTo(t, WebVitalsList(7, nil, PerfFilter{}, nil, "u@e.com", false))
	if strings.Contains(empty, "/home") {
		t.Error("пустой web-vitals")
	}
}

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
		Issue:    trace.PerfIssue{ID: 1, Kind: trace.KindNPlusOne, Title: "N+1", Culprit: "db", Status: "unresolved", Count: 12, SampleTraceID: "t1"},
		Evidence: PerfEvidence{Count: 12, TotalUS: 120000, MaxUS: 30000, ParentOp: "http.server", SequentialPct: 80, MaxConcurrency: 1, URLs: []string{"/a", "/b"}, HasTotal: true, HasMax: true, HasSequential: true},
	}
	outD := renderTo(t, PerfIssueDetail(d, "u@e.com"))
	if !strings.Contains(outD, "N+1") {
		t.Error("деталь perf-issue должна содержать заголовок")
	}
}

func TestIncidentsAndRegressionsLists(t *testing.T) {
	now := time.Now()
	incRows := []IncidentRow{
		{Incident: uptime.Incident{ID: 1, StartedAt: now.Add(-time.Hour), Cause: "timeout"}, MonitorName: "web"},
		{Incident: uptime.Incident{ID: 2, StartedAt: now.Add(-5 * time.Hour), ResolvedAt: ptrTime(now.Add(-4 * time.Hour)), Cause: "5xx"}, MonitorName: "api"},
	}
	out := renderTo(t, IncidentsList(7, incRows, 1, int64(len(incRows)), "u@e.com"))
	if !strings.Contains(out, "web") || !strings.Contains(out, "api") {
		t.Error("инциденты должны содержать имена мониторов")
	}

	regs := []trace.Regression{
		{ID: 1, TargetKind: "endpoint_p95", Target: "GET /api", Metric: "duration", Status: "open", BaselineValue: 100, PeakValue: 300, CurrentValue: 250, StartedAt: now.Add(-time.Hour)},
		{ID: 2, TargetKind: "webvital_p75", Target: "/home", Metric: "lcp", Status: "resolved", BaselineValue: 2000, PeakValue: 4000, StartedAt: now.Add(-3 * time.Hour), ResolvedAt: ptrTime(now.Add(-time.Hour))},
	}
	outR := renderTo(t, RegressionsList(7, regs, nil, "open", "u@e.com", false, true))
	if !strings.Contains(outR, "GET /api") {
		t.Error("регрессии должны содержать цель")
	}
}

func TestRegressionsListSeasonalBadge(t *testing.T) {
	now := time.Now()
	regs := []trace.Regression{
		{ID: 1, TargetKind: "endpoint_p95", Target: "GET /api", Metric: "duration", Status: "open", BaselineValue: 100, PeakValue: 300, StartedAt: now.Add(-time.Hour)},
	}
	on := renderTo(t, RegressionsList(7, regs, nil, "open", "u@e.com", true, true))
	if !strings.Contains(on, "Сезонный режим") {
		t.Error("при seasonal=true должен быть бейдж «Сезонный режим»")
	}
	off := renderTo(t, RegressionsList(7, regs, nil, "open", "u@e.com", false, true))
	if strings.Contains(off, "Сезонный режим") {
		t.Error("при seasonal=false бейджа быть не должно")
	}
}

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

func TestProjectSettings(t *testing.T) {
	project := org.Project{ID: 7, OrgID: 1, Slug: "web", Name: "Web", Platform: "go"}
	keys := []ProjectKeyView{
		{Key: org.Key{ID: 1, PublicKey: "pk_live", Kind: org.KindServer, Revoked: false}, DSN: "https://pk_live@dsn"},
		{Key: org.Key{ID: 2, PublicKey: "pk_old", Kind: org.KindLegacy, Revoked: true}, DSN: "https://pk_old@dsn"},
		// Kind=="" — строки без миграции типов; keyKindLabelKey должна показать «без типа», как и явный legacy.
		{Key: org.Key{ID: 3, PublicKey: "pk_untyped", Kind: "", Revoked: false}, DSN: "https://pk_untyped@dsn"},
	}
	perf := PerfSettingsForm{SampleRate: "1.0", ApdexMS: "500", NPlusOneMin: "5", SlowDBMs: "300"}
	reg := RegressionSettingsForm{ThresholdPct: "20", RecoveryPct: "10", WindowMinutes: "60", MinSamples: "100", Enabled: true}
	out := renderTo(t, ProjectSettings(project, keys, "", "u@e.com", perf, reg, 30, nil))
	if !strings.Contains(out, "pk_live") || !strings.Contains(out, "badge-danger") {
		t.Error("ключи со статусами должны отрендериться")
	}
	if !strings.Contains(out, "https://pk_live@dsn") {
		t.Error("собственный DSN живого ключа должен отрендериться")
	}
	if strings.Contains(out, "https://pk_old@dsn") {
		t.Error("отозванный ключ не должен показывать DSN")
	}
	if !strings.Contains(out, "/docs/keys") {
		t.Error("legacy-ключ должен показывать ссылку на /docs/keys")
	}
	if !strings.Contains(out, "https://pk_untyped@dsn") {
		t.Error("ключ с Kind==\"\" должен показывать собственный DSN")
	}
	// бейдж ищем в карточке именно этого ключа — pk_old (legacy) тоже несёт badge-warn.
	idx := strings.Index(out, "pk_untyped")
	if idx == -1 {
		t.Fatal("ключ с Kind==\"\" не найден в выводе")
	}
	cardStart := strings.LastIndex(out[:idx], `<article class="key-card`)
	cardEnd := strings.Index(out[idx:], "</article>")
	if cardStart == -1 || cardEnd == -1 {
		t.Fatalf("malformed card for pk_untyped: %s", out)
	}
	card := out[cardStart : idx+cardEnd+len("</article>")]
	if !strings.Contains(card, "badge-warn") {
		t.Error("ключ с Kind==\"\" должен получить предупреждающий бейдж legacy, а не обычный")
	}
	if !strings.Contains(card, "/docs/keys") {
		t.Error("ключ с Kind==\"\" должен показывать ссылку на /docs/keys в своей карточке")
	}
}

func TestProjectsListAndNoProjects(t *testing.T) {
	items := []ProjectListItem{
		{Project: org.Project{ID: 1, Name: "web", Slug: "web", Platform: "go"}, CanManage: true},
		{Project: org.Project{ID: 2, Name: "api", Slug: "api", Platform: "php"}, CanManage: false},
	}
	out := renderTo(t, ProjectsList(items, []OrgOption{{ID: 1, Name: "Acme"}}, nil, "", "u@e.com"))
	if !strings.Contains(out, "web") || !strings.Contains(out, "api") {
		t.Error("список проектов должен содержать имена")
	}
	// владельцу/админу — CTA в создание проекта, участнику — текст без CTA; выход доступен обоим.
	np := renderTo(t, NoProjects(true, "u@e.com"))
	if !strings.Contains(np, `href="/projects"`) || !strings.Contains(np, `action="/logout"`) {
		t.Error("админский экран без проектов должен вести в /projects и давать выход")
	}
	npMember := renderTo(t, NoProjects(false, "u@e.com"))
	if strings.Contains(npMember, `href="/projects"`) {
		t.Error("участник без прав не должен видеть CTA создания проекта")
	}
	if !strings.Contains(npMember, `action="/logout"`) {
		t.Error("экран участника должен давать выход")
	}
}

func TestOnboarding(t *testing.T) {
	out := renderTo(t, Onboarding("занятый slug", "acme", "Acme", "web", "Web", "go", "u@e.com"))
	if !strings.Contains(out, "занятый slug") || !strings.Contains(out, "Acme") {
		t.Error("онбординг должен показать ошибку и значения")
	}
}

func TestAlertDeliveries(t *testing.T) {
	failed := []notify.FailedJob{
		{ID: 1, ChannelKind: "email", Target: "a@b.c", LastError: strings.Repeat("x", 400), Attempts: 5, CreatedAt: time.Now()},
	}
	out := renderTo(t, AlertDeliveries(7, failed, true, "u@e.com"))
	if !strings.Contains(out, "a@b.c") {
		t.Error("упавшие доставки должны показать цель")
	}
	empty := renderTo(t, AlertDeliveries(7, nil, true, "u@e.com"))
	if len(empty) == 0 {
		t.Error("пустой журнал доставок всё равно рендерится")
	}
}

// не-admin видит подсказку про маскировку рядом с таблицей, admin (canManage) — нет.
func TestAlertDeliveriesMaskedHint(t *testing.T) {
	failed := []notify.FailedJob{
		{ID: 1, ChannelKind: "email", Target: "a@b.c", CreatedAt: time.Now()},
	}
	hint := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "alerts.channels.target_masked_hint")
	masked := renderTo(t, AlertDeliveries(7, failed, false, "u@e.com"))
	if !strings.Contains(masked, hint) {
		t.Error("не-admin должен увидеть подсказку про маскировку")
	}
	unmasked := renderTo(t, AlertDeliveries(7, failed, true, "u@e.com"))
	if strings.Contains(unmasked, hint) {
		t.Error("admin (canManage) не должен видеть подсказку про маскировку")
	}
}

func TestProfilePage(t *testing.T) {
	linked := []LinkedIdentity{{Provider: "yandex", DisplayName: "Яндекс", Email: "u@ya.ru", CanUnlink: true}}
	linkable := []LinkableProvider{{Name: "github", DisplayName: "GitHub"}}
	out := renderTo(t, Profile("u@e.com", "", "сохранено", true, linked, linkable, false, "u@e.com"))
	if !strings.Contains(out, "Яндекс") || !strings.Contains(out, "GitHub") {
		t.Error("провайдеры должны отрендериться")
	}
	if !strings.Contains(out, "сохранено") {
		t.Error("сообщение должно отрендериться")
	}
	if strings.Contains(out, "/profile/instance-admin/transfer") {
		t.Error("не-админу инстанса секция передачи роли не должна показываться")
	}
	outErr := renderTo(t, Profile("u@e.com", "ошибка", "", false, nil, linkable, false, "u@e.com"))
	if !strings.Contains(outErr, "ошибка") {
		t.Error("ошибка профиля должна отрендериться")
	}
}

func TestProfilePageInstanceAdmin(t *testing.T) {
	out := renderTo(t, Profile("u@e.com", "", "", true, nil, nil, true, "u@e.com"))
	if !strings.Contains(out, "/profile/instance-admin/transfer") {
		t.Error("администратору инстанса секция передачи роли должна показываться")
	}
}

// пейджер обязан остаться на пустой out-of-range странице — иначе пользователь застревает без пути назад.
func TestIncidentsPagerOnEmptyPage(t *testing.T) {
	out := renderTo(t, IncidentsList(7, nil, 5, 0, "u@e.com"))
	if !strings.Contains(out, `class="pagination"`) {
		t.Fatalf("на пустой out-of-range странице нет пейджера:\n%s", out)
	}
	// первая страница — базовый URL без ?page.
	if !strings.Contains(out, `href="/projects/7/incidents"`) {
		t.Errorf("ссылка «назад» должна вести на первую страницу:\n%s", out)
	}
	if strings.Contains(out, "5 / 0") {
		t.Errorf("не должно быть счётчика при total=0:\n%s", out)
	}
}

// заглушка issues.untitled обязана попасть и в aria-label чекбокса, и в ссылку списка, и в <h1>/<title> детали.
func TestIssuesUntitledFallback(t *testing.T) {
	untitled := i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), "issues.untitled")
	if untitled == "" || untitled == "issues.untitled" {
		t.Fatalf("ключ issues.untitled должен быть в каталоге: %q", untitled)
	}
	now := time.Now()

	rows := []IssueRow{{Issue: issue.Issue{ID: 9, Title: "", Level: "error", Status: "unresolved", TimesSeen: 1, FirstSeen: now, LastSeen: now}, Sparkline: stub()}}
	list := renderTo(t, IssuesList(7, rows, IssuesFilter{Status: "unresolved"}, 1, 1, "u@e.com", nil, nil, GettingStartedVM{ProjectID: 7, Done: 3, Step2Done: true}, true, true, false))
	if strings.Contains(list, `aria-label=""`) {
		t.Error("список: пустой aria-label у чекбокса массовых действий")
	}
	if !strings.Contains(list, `aria-label="`+untitled+`"`) {
		t.Errorf("список: aria-label чекбокса должен нести заглушку %q", untitled)
	}
	if !strings.Contains(list, `">`+untitled+`</a>`) {
		t.Errorf("список: ссылка на issue должна нести заглушку %q", untitled)
	}

	it := issue.Issue{ID: 9, Title: "", Level: "error", Status: "unresolved", TimesSeen: 1, FirstSeen: now, LastSeen: now}
	ev := event.Stored{ID: "ev1", Level: "error", Message: ""}
	detail := renderTo(t, IssueDetail(it, nil, stub(), TimeRangeVM{Key: "24h"}, []event.Stored{ev}, "ev1", &ev, nil, "u@e.com", false, false, "", "", true, true, false))
	if strings.Contains(detail, "<h1></h1>") {
		t.Error("деталь: пустой <h1>")
	}
	if !strings.Contains(detail, "<h1>"+untitled+"</h1>") {
		t.Errorf("деталь: <h1> должен нести заглушку %q", untitled)
	}
	if !strings.Contains(detail, "<title>"+untitled+" · Gotcha</title>") {
		t.Errorf("деталь: <title> должен нести заглушку %q", untitled)
	}

	rows[0].Issue.Title = "NPE"
	list = renderTo(t, IssuesList(7, rows, IssuesFilter{Status: "unresolved"}, 1, 1, "u@e.com", nil, nil, GettingStartedVM{ProjectID: 7, Done: 3, Step2Done: true}, true, true, false))
	if !strings.Contains(list, `aria-label="NPE"`) || strings.Contains(list, untitled) {
		t.Error("список: непустой заголовок должен рисоваться как есть")
	}
}

// видна только при !Hidden && Done < 5 && CanOperate.
// CanOperate раньше был недостижим по построению — теперь predicate обязан держать каждую ветку сам.
func TestChecklistVisibleTable(t *testing.T) {
	cases := []struct {
		name       string
		hidden     bool
		done       int
		canOperate bool
		want       bool
	}{
		{"visible: fresh, operator", false, 0, true, true},
		{"visible: 4 of 5, operator", false, 4, true, true},
		{"hidden by user", true, 0, true, false},
		{"all steps done", false, 5, true, false},
		{"more than all steps done", false, 6, true, false},
		{"not operator", false, 0, false, false},
		{"not operator, 4 of 5", false, 4, false, false},
		{"hidden and not operator", true, 0, false, false},
		{"hidden and done", true, 5, true, false},
		{"done and not operator", false, 5, false, false},
		{"everything against", true, 5, false, false},
	}
	for _, tc := range cases {
		gs := GettingStartedVM{Hidden: tc.hidden, Done: tc.done, CanOperate: tc.canOperate}
		if got := gs.checklistVisible(); got != tc.want {
			t.Errorf("%s: checklistVisible(Hidden=%v, Done=%d, CanOperate=%v) = %v, want %v",
				tc.name, tc.hidden, tc.done, tc.canOperate, got, tc.want)
		}
	}
}
