package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// ruCtx — контекст с русской локалью для хелперов, зовущих i18n.T.
func ruCtx() context.Context {
	return i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
}

// TestLevelBadgeClass — каждый уровень issue красится своим бейджем; error и
// fatal делят «опасный», всё неизвестное падает в нейтральный.
func TestLevelBadgeClass(t *testing.T) {
	cases := map[string]string{
		"error":   "badge badge-danger",
		"fatal":   "badge badge-danger",
		"warning": "badge badge-warn",
		"info":    "badge badge-info",
		"debug":   "badge badge-neutral",
		"":        "badge badge-neutral",
	}
	for in, want := range cases {
		if got := levelBadgeClass(in); got != want {
			t.Errorf("levelBadgeClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStatusBadgeClass — статус issue: resolved «хорошо», ignored нейтральный,
// всё прочее (в т.ч. unresolved) требует внимания (warn).
func TestStatusBadgeClass(t *testing.T) {
	cases := map[string]string{
		"resolved":   "badge badge-good",
		"ignored":    "badge badge-neutral",
		"unresolved": "badge badge-warn",
		"":           "badge badge-warn",
	}
	for in, want := range cases {
		if got := statusBadgeClass(in); got != want {
			t.Errorf("statusBadgeClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMonitorStatusBadgeClass — все статусы монитора имеют свой бейдж, а
// неизвестный статус деградирует в warn, а не в панику.
func TestMonitorStatusBadgeClass(t *testing.T) {
	cases := map[string]string{
		"up":          "badge badge-good",
		"down":        "badge badge-danger",
		"paused":      "badge badge-neutral",
		"maintenance": "badge badge-info",
		"unknown":     "badge badge-warn",
	}
	for in, want := range cases {
		if got := monitorStatusBadgeClass(in); got != want {
			t.Errorf("monitorStatusBadgeClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestVitalBadgeClass — рейтинг web-vital → класс бейджа; неизвестный рейтинг
// не даёт класса вовсе (пустая строка).
func TestVitalBadgeClass(t *testing.T) {
	cases := map[string]string{
		"good":              "badge badge-good",
		"needs-improvement": "badge badge-warn",
		"poor":              "badge badge-danger",
		"":                  "",
	}
	for in, want := range cases {
		if got := vitalBadgeClass(in); got != want {
			t.Errorf("vitalBadgeClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPerfKindBadgeClass — вид perf-проблемы: N+1 предупреждает, медленный
// запрос информирует, неизвестный вид нейтрален.
func TestPerfKindBadgeClass(t *testing.T) {
	cases := map[string]string{
		trace.KindNPlusOne:    "badge badge-warn",
		trace.KindSlowDBQuery: "badge badge-info",
		trace.KindHTTPFlood:   "badge badge-neutral",
	}
	for in, want := range cases {
		if got := perfKindBadgeClass(in); got != want {
			t.Errorf("perfKindBadgeClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPerfStatusBadgeClass — статус perf-issue: resolved хорошо, ignored
// нейтрально, открытый — опасный.
func TestPerfStatusBadgeClass(t *testing.T) {
	cases := map[string]string{
		"resolved":   "badge badge-good",
		"ignored":    "badge badge-neutral",
		"unresolved": "badge badge-danger",
	}
	for in, want := range cases {
		if got := perfStatusBadgeClass(in); got != want {
			t.Errorf("perfStatusBadgeClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCheckStatusBadgeClass — булев статус проверки: ok зелёный, fail красный.
func TestCheckStatusBadgeClass(t *testing.T) {
	if got := checkStatusBadgeClass(true); got != "badge badge-good" {
		t.Errorf("checkStatusBadgeClass(true) = %q", got)
	}
	if got := checkStatusBadgeClass(false); got != "badge badge-danger" {
		t.Errorf("checkStatusBadgeClass(false) = %q", got)
	}
}

// TestIncidentStatusBadgeClass — незакрытый инцидент (ResolvedAt=nil) опасен,
// закрытый — «хорошо».
func TestIncidentStatusBadgeClass(t *testing.T) {
	open := uptime.Incident{}
	if got := incidentStatusBadgeClass(open); got != "badge badge-danger" {
		t.Errorf("open incident badge = %q", got)
	}
	now := time.Now()
	closed := uptime.Incident{ResolvedAt: &now}
	if got := incidentStatusBadgeClass(closed); got != "badge badge-good" {
		t.Errorf("closed incident badge = %q", got)
	}
}

// TestIncidentBadgeClass — метричный инцидент: open красный, всё прочее хорошо.
func TestIncidentBadgeClass(t *testing.T) {
	if got := incidentBadgeClass("open"); got != "badge badge-danger" {
		t.Errorf("incidentBadgeClass(open) = %q", got)
	}
	if got := incidentBadgeClass("resolved"); got != "badge badge-good" {
		t.Errorf("incidentBadgeClass(resolved) = %q", got)
	}
}

// TestProbeStatusBadgeClass — статус пробы online/offline/иное.
func TestProbeStatusBadgeClass(t *testing.T) {
	cases := map[string]string{
		"online":  "badge badge-good",
		"offline": "badge badge-danger",
		"idle":    "badge badge-neutral",
	}
	for in, want := range cases {
		if got := probeStatusBadgeClass(in); got != want {
			t.Errorf("probeStatusBadgeClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRegressionStatusBadgeClass — открытая регрессия опасна, решённая хороша.
func TestRegressionStatusBadgeClass(t *testing.T) {
	if got := regressionStatusBadgeClass("open"); got != "badge badge-danger" {
		t.Errorf("open = %q", got)
	}
	if got := regressionStatusBadgeClass("resolved"); got != "badge badge-good" {
		t.Errorf("resolved = %q", got)
	}
}

// TestMemberRoleBadgeClass — роли: owner выделяется warn, admin info, member
// нейтрально.
func TestMemberRoleBadgeClass(t *testing.T) {
	cases := map[org.Role]string{
		org.RoleOwner:  "badge badge-warn",
		org.RoleAdmin:  "badge badge-info",
		org.RoleMember: "badge badge-neutral",
	}
	for in, want := range cases {
		if got := memberRoleBadgeClass(in); got != want {
			t.Errorf("memberRoleBadgeClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestKeyStatusBadgeClass — отозванный ключ красный, активный зелёный.
func TestKeyStatusBadgeClass(t *testing.T) {
	if got := keyStatusBadgeClass(org.Key{Revoked: true}); got != "badge badge-danger" {
		t.Errorf("revoked = %q", got)
	}
	if got := keyStatusBadgeClass(org.Key{}); got != "badge badge-good" {
		t.Errorf("active = %q", got)
	}
}

// TestOverallStatusClass — сводный статус страницы: major/partial/ok.
func TestOverallStatusClass(t *testing.T) {
	cases := map[string]string{
		"major":       "status-overall status-overall-major",
		"partial":     "status-overall status-overall-partial",
		"operational": "status-overall status-overall-ok",
	}
	for in, want := range cases {
		if got := overallStatusClass(in); got != want {
			t.Errorf("overallStatusClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFormatDurationUS — единица длительности подбирается по величине:
// микро/милли/секунды, а ноль показывается голым «0».
func TestFormatDurationUS(t *testing.T) {
	cases := []struct {
		us   uint32
		want string
	}{
		{0, "0"},
		{500, "500µs"},
		{1500, "1.5ms"},
		{2_500_000, "2.50s"},
	}
	for _, c := range cases {
		if got := formatDurationUS(c.us); got != c.want {
			t.Errorf("formatDurationUS(%d) = %q, want %q", c.us, got, c.want)
		}
	}
}

// TestFormatApdexAndFailureRate — apdex это два знака после запятой, а failure
// rate — процент с одним знаком и суффиксом «%».
func TestFormatApdexAndFailureRate(t *testing.T) {
	if got := formatApdex(0.937); got != "0.94" {
		t.Errorf("formatApdex = %q", got)
	}
	if got := formatFailureRate(0.05); got != "5.0%" {
		t.Errorf("formatFailureRate = %q", got)
	}
}

// TestFormatVitalMSAndValue — vital в мс до секунды показывается целыми мс,
// свыше — секундами; CLS особый (безразмерный, 2 знака); пустой рейтинг → «—».
func TestFormatVitalMSAndValue(t *testing.T) {
	if got := formatVitalMS(250); got != "250ms" {
		t.Errorf("formatVitalMS(250) = %q", got)
	}
	if got := formatVitalMS(2500); got != "2.50s" {
		t.Errorf("formatVitalMS(2500) = %q", got)
	}
	if got := formatVitalValue(trace.Vital{}); got != "—" {
		t.Errorf("empty vital = %q", got)
	}
	cls := trace.Vital{Name: "cls", Rating: "good", P75: 0.08}
	if got := formatVitalValue(cls); got != "0.08" {
		t.Errorf("cls vital = %q", got)
	}
	lcp := trace.Vital{Name: "lcp", Rating: "poor", P75: 4200}
	if got := formatVitalValue(lcp); got != "4.20s" {
		t.Errorf("lcp vital = %q", got)
	}
}

// TestVitalLabel — сырые имена приводятся к аббревиатурам верхним регистром,
// неизвестное имя возвращается как есть.
func TestVitalLabel(t *testing.T) {
	cases := map[string]string{
		"lcp": "LCP", "inp": "INP", "cls": "CLS",
		"fcp": "FCP", "ttfb": "TTFB", "custom": "custom",
	}
	for in, want := range cases {
		if got := vitalLabel(in); got != want {
			t.Errorf("vitalLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestComparatorSymbol — lt это «<», всё остальное «>».
func TestComparatorSymbol(t *testing.T) {
	if comparatorSymbol("lt") != "<" || comparatorSymbol("gt") != ">" {
		t.Fatal("comparatorSymbol сломан")
	}
}

// TestMetricUnitText — пустая единица и безразмерная «1» показываются как «—».
func TestMetricUnitText(t *testing.T) {
	if metricUnitText("") != "—" || metricUnitText("1") != "—" {
		t.Fatal("пустая/безразмерная единица должна быть —")
	}
	if metricUnitText("ms") != "ms" {
		t.Fatal("реальная единица должна показываться как есть")
	}
}

// TestInitialsAndEmailLocal — инициалы берут до двух букв локальной части,
// emailLocal отрезает домен, а без «@» возвращает вход как есть.
func TestInitialsAndEmailLocal(t *testing.T) {
	if got := initials("demo@gotcha.local"); got != "DE" {
		t.Errorf("initials = %q", got)
	}
	if got := initials("x@y.z"); got != "X" {
		t.Errorf("single-rune initials = %q", got)
	}
	if got := emailLocal("demo@gotcha.local"); got != "demo" {
		t.Errorf("emailLocal = %q", got)
	}
	if got := emailLocal("noatsign"); got != "noatsign" {
		t.Errorf("emailLocal без @ = %q", got)
	}
}

// TestAssigneeDisplay — пустой назначенный показывается как «—».
func TestAssigneeDisplay(t *testing.T) {
	if assigneeDisplay("") != "—" {
		t.Fatal("пустой assignee должен быть —")
	}
	if assigneeDisplay("a@b.c") != "a@b.c" {
		t.Fatal("непустой assignee показывается как есть")
	}
}

// TestFrameLocation — кадр без файла не даёт локации; с файлом — «файл:строка».
func TestFrameLocation(t *testing.T) {
	if got := frameLocation(Frame{}); got != "" {
		t.Errorf("empty frame location = %q", got)
	}
	if got := frameLocation(Frame{Filename: "main.go", Lineno: 42}); got != "main.go:42" {
		t.Errorf("frame location = %q", got)
	}
}

// TestTruncateFailedError — длинная ошибка обрезается многоточием, короткая —
// нет.
func TestTruncateFailedError(t *testing.T) {
	short := "boom"
	if truncateFailedError(short) != short {
		t.Fatal("короткая ошибка не должна обрезаться")
	}
	long := strings.Repeat("я", 500)
	got := truncateFailedError(long)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) >= 500 {
		t.Fatalf("длинная ошибка должна обрезаться с …: len=%d", len([]rune(got)))
	}
}

// TestUptimeStatText — без проверок «no data», иначе процент с двумя знаками.
func TestUptimeStatText(t *testing.T) {
	if got := uptimeStatText(uptime.UptimeStat{}); got != "no data" {
		t.Errorf("empty = %q", got)
	}
	if got := uptimeStatText(uptime.UptimeStat{Total: 100, OK: 99}); got != "99.00%" {
		t.Errorf("99/100 = %q", got)
	}
}

// TestAvgLatencyText — без проверок «-», иначе «<ms>ms».
func TestAvgLatencyText(t *testing.T) {
	if got := avgLatencyText(uptime.UptimeStat{}, 120); got != "-" {
		t.Errorf("empty = %q", got)
	}
	if got := avgLatencyText(uptime.UptimeStat{Total: 5}, 120); got != "120ms" {
		t.Errorf("with data = %q", got)
	}
}

// TestProfileWeightByType — nanos-типы форматируются временем, bytes-типы
// весом, неизвестный тип — голым числом.
func TestProfileWeightByType(t *testing.T) {
	cases := []struct {
		typ    string
		weight uint64
		want   string
	}{
		{"cpu", 2_000_000_000, "2.00s"},
		{"heap", 2 * 1024 * 1024, "2.0MB"},
		{"samples", 42, "42"},
	}
	for _, c := range cases {
		if got := profileWeightByType(c.typ, c.weight); got != c.want {
			t.Errorf("profileWeightByType(%q,%d) = %q, want %q", c.typ, c.weight, got, c.want)
		}
	}
}

// TestFormatProfileBytesGB — гигабайтная ветка (в profiles_test не покрыта).
func TestFormatProfileBytesGB(t *testing.T) {
	if got := formatProfileBytes(3 * 1024 * 1024 * 1024); got != "3.00GB" {
		t.Errorf("formatProfileBytes GB = %q", got)
	}
	if got := formatProfileBytes(512); got != "512B" {
		t.Errorf("formatProfileBytes B = %q", got)
	}
}

// TestFormatProfileNanosNs — суб-микросекундная ветка (голые ns).
func TestFormatProfileNanosNs(t *testing.T) {
	if got := formatProfileNanos(500); got != "500ns" {
		t.Errorf("formatProfileNanos ns = %q", got)
	}
}

// TestTotalPages — total<=0 всегда одна страница, иначе округление вверх.
func TestTotalPages(t *testing.T) {
	if totalPages(0) != 1 {
		t.Fatal("пустой список = 1 страница")
	}
	if totalPages(1) != 1 {
		t.Fatal("одна запись = 1 страница")
	}
	// issuesPerPage записей должны уложиться ровно в одну страницу, +1 — во вторую.
	one := totalPages(int64(issuesPerPage))
	two := totalPages(int64(issuesPerPage) + 1)
	if two != one+1 {
		t.Fatalf("округление вверх сломано: %d vs %d", one, two)
	}
}

// TestIssuesPageURL — фильтры кладутся в query; пустой фильтр даёт голый путь;
// первая страница не пишет page, вторая — пишет.
func TestIssuesPageURL(t *testing.T) {
	if got := issuesPageURL(7, IssuesFilter{}, 1); strings.Contains(got, "?") {
		t.Errorf("пустой фильтр не должен давать query: %q", got)
	}
	f := IssuesFilter{Status: "resolved", Level: "error", Query: "boom", Sort: "freq", Environment: "prod", Period: "7d"}
	got := issuesPageURL(7, f, 2)
	for _, want := range []string{"status=resolved", "level=error", "q=boom", "sort=freq", "env=prod", "period=7d", "page=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("issuesPageURL пропустил %q: %q", want, got)
		}
	}
}

// TestRegressionIncreasePct — рост от базы в процентах; нулевая база → «—».
func TestRegressionIncreasePct(t *testing.T) {
	if got := regressionIncreasePct(trace.Regression{}); got != "—" {
		t.Errorf("нулевая база = %q", got)
	}
	r := trace.Regression{BaselineValue: 100, PeakValue: 150}
	if got := regressionIncreasePct(r); got != "+50%" {
		t.Errorf("рост = %q", got)
	}
}

// TestRegressionValueRange — диапазон «база → пик» с учётом метрики (cls особый).
func TestRegressionValueRange(t *testing.T) {
	r := trace.Regression{Metric: "cls", BaselineValue: 0.05, PeakValue: 0.30}
	if got := regressionValueRange(r); got != "0.05 → 0.30" {
		t.Errorf("cls range = %q", got)
	}
	rd := trace.Regression{Metric: "duration", BaselineValue: 100, PeakValue: 2500}
	if got := regressionValueRange(rd); got != "100ms → 2.50s" {
		t.Errorf("duration range = %q", got)
	}
}

// TestMembershipHelpers — фильтры видимости для команд/проектов.
func TestMembershipHelpers(t *testing.T) {
	members := []org.Member{{UserID: 1, Role: org.RoleOwner}, {UserID: 2, Role: org.RoleMember}}
	if !memberInTeam(members, 1) || memberInTeam(members, 9) {
		t.Fatal("memberInTeam сломан")
	}
	if !viewerIsOwner(members, 1) || viewerIsOwner(members, 2) || viewerIsOwner(members, 9) {
		t.Fatal("viewerIsOwner сломан")
	}
	projects := []org.Project{{ID: 10}, {ID: 20}}
	if !projectAttached(projects, 10) || projectAttached(projects, 99) {
		t.Fatal("projectAttached сломан")
	}
	// В команде уже есть участник 1 → доступен ещё участник 2.
	if !hasAvailableMembers(members, []org.Member{{UserID: 1}}) {
		t.Fatal("должен найтись свободный участник")
	}
	if hasAvailableMembers(members, members) {
		t.Fatal("все участники уже в команде — свободных нет")
	}
}

// TestYesNoLocalized — булев ответ локализуется (ru), да ≠ нет.
func TestYesNoLocalized(t *testing.T) {
	ctx := ruCtx()
	yes, no := yesNo(ctx, true), yesNo(ctx, false)
	if yes == "" || no == "" || yes == no {
		t.Fatalf("yesNo сломан: yes=%q no=%q", yes, no)
	}
}

// TestErrorKeys — ключи заголовка/тела ошибки заданы для 403/404/500 и пусты
// для прочих статусов.
func TestErrorKeys(t *testing.T) {
	for _, s := range []int{403, 404, 500} {
		if errorTitleKey(s) == "" || errorBodyKey(s) == "" {
			t.Errorf("нет ключей для статуса %d", s)
		}
	}
	if errorTitleKey(418) != "" || errorBodyKey(418) != "" {
		t.Error("неизвестный статус должен давать пустые ключи")
	}
}
