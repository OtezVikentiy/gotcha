package web

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestCoverPathHelpers дёргает тривиальные path-хелперы, которые иначе
// покрываются только через шаблоны (другой пакет) — прямой вызов проверяет,
// что они собирают ожидаемый префикс.
func TestCoverPathHelpers(t *testing.T) {
	cases := map[string]string{
		orgSettingsRolePath(1):           "/orgs/1/settings/role",
		orgSettingsRemovePath(1):         "/orgs/1/settings/remove",
		orgSettingsInvitePath(1):         "/orgs/1/settings/invite",
		orgSettingsQuotaPath(1):          "/orgs/1/settings/quota",
		orgSettingsPurgeSubjectPath(1):   "/orgs/1/settings/purge-subject",
		orgSettingsExportSubjectPath(1):  "/orgs/1/settings/export-subject",
		teamMembersPath(2):               "/teams/2/members",
		teamMembersRemovePath(2):         "/teams/2/members/remove",
		teamProjectsPath(2):              "/teams/2/projects",
		teamProjectsDetachPath(2):        "/teams/2/projects/detach",
		performancePath(3):               "/projects/3/performance",
		perfIssuesPath(3):                "/projects/3/perf-issues",
		profilesPath(3):                  "/projects/3/profiles",
		profileRegressionsPath(3):        "/projects/3/profile-regressions",
		regressionsPath(3):               "/projects/3/regressions",
		webVitalsPath(3):                 "/projects/3/web-vitals",
		projectSettingsRenamePath(3):     "/projects/3/settings/rename",
		projectSettingsPerformancePath(3): "/projects/3/settings/performance",
		projectSettingsRegressionsPath(3): "/projects/3/settings/regressions",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path helper = %q, want %q", got, want)
		}
	}
}

// TestCoverErrorMessages прогоняет каждую доменную ошибку через переводчики
// сообщений — так покрываются все case-ветки switch'ей за один заход.
func TestCoverErrorMessages(t *testing.T) {
	ctx := context.Background()
	dummy := context.DeadlineExceeded // «прочая» ошибка → ветка default

	orgErrs := []error{org.ErrLastOwner, org.ErrInvalidRole, org.ErrNotMember, org.ErrOwnerOnly, org.ErrInvalidQuota, dummy}
	for _, e := range orgErrs {
		if orgSettingsErrorMessage(ctx, e) == "" {
			t.Errorf("orgSettingsErrorMessage(%v) empty", e)
		}
	}

	for _, e := range []error{uptime.ErrSlugTaken, uptime.ErrInvalidStatusPage, dummy} {
		if statusPageErrorMessage(ctx, e) == "" {
			t.Errorf("statusPageErrorMessage(%v) empty", e)
		}
	}
	for _, e := range []error{uptime.ErrInvalidMonitor, dummy} {
		if monitorFormErrorMessage(ctx, e) == "" {
			t.Errorf("monitorFormErrorMessage(%v) empty", e)
		}
	}
	for _, e := range []error{uptime.ErrInvalidWindow, dummy} {
		if maintenanceErrorMessage(ctx, e) == "" {
			t.Errorf("maintenanceErrorMessage(%v) empty", e)
		}
	}
	for _, e := range []error{org.ErrInvalidSlug, org.ErrSlugTaken, org.ErrNotMember, errCrossOrgProject, dummy} {
		if teamsErrorMessage(ctx, e) == "" {
			t.Errorf("teamsErrorMessage(%v) empty", e)
		}
	}
	for _, e := range []error{org.ErrInvalidName, dummy} {
		if projectSettingsErrorMessage(ctx, e) == "" {
			t.Errorf("projectSettingsErrorMessage(%v) empty", e)
		}
	}
	for _, e := range []error{org.ErrInvalidSlug, org.ErrSlugTaken, dummy} {
		if onboardingErrorMessage(ctx, e) == "" {
			t.Errorf("onboardingErrorMessage(%v) empty", e)
		}
	}
	for _, e := range []error{alert.ErrInvalidRule, alert.ErrInvalidChannel, dummy} {
		if alertsErrorMessage(ctx, e) == "" {
			t.Errorf("alertsErrorMessage(%v) empty", e)
		}
	}
	for _, e := range []error{auth.ErrEmailTaken, auth.ErrWeakPassword, auth.ErrInvalidEmail, dummy} {
		if registerErrorMessage(ctx, e) == "" {
			t.Errorf("registerErrorMessage(%v) empty", e)
		}
	}
	for _, e := range []error{auth.ErrInvalidCredentials, auth.ErrWeakPassword, dummy} {
		if profilePasswordErrorMessage(ctx, e) == "" {
			t.Errorf("profilePasswordErrorMessage(%v) empty", e)
		}
	}
}

// TestCoverPeriodAndStepHelpers покрывает нормализацию периода/шага для страниц
// перформанса и профилей и форматирование шага.
func TestCoverPeriodAndStepHelpers(t *testing.T) {
	// perfPeriodWindow: известный период и дефолт.
	if _, name := perfPeriodWindow("24h"); name == "" {
		t.Error("perfPeriodWindow(24h) empty name")
	}
	if _, name := perfPeriodWindow("bogus-period"); name == "" {
		t.Error("perfPeriodWindow(bogus) empty name")
	}
	// perfBucketStep: окно, требующее округления до 5 минут, и слишком маленькое.
	if step := perfBucketStep(23*time.Hour, 24); step%(5*time.Minute) != 0 {
		t.Errorf("perfBucketStep not multiple of 5m: %v", step)
	}
	if step := perfBucketStep(time.Minute, 24); step != 5*time.Minute {
		t.Errorf("perfBucketStep(tiny) = %v, want 5m floor", step)
	}
	// profilePeriodWindow: все три ветки.
	for _, p := range []string{"1h", "7d", "24h", "other"} {
		if _, name := profilePeriodWindow(p); name == "" {
			t.Errorf("profilePeriodWindow(%q) empty", p)
		}
	}
	// formatStep: часы, минуты, секунды.
	for _, d := range []time.Duration{2 * time.Hour, 30 * time.Minute, 15 * time.Second} {
		if formatStep(d) == "" {
			t.Errorf("formatStep(%v) empty", d)
		}
	}
	// waterfallMS: µs, ms, s.
	for _, us := range []uint32{500, 5000, 5_000_000} {
		if waterfallMS(us) == "" {
			t.Errorf("waterfallMS(%d) empty", us)
		}
	}
}

// TestCoverSortHelpers покрывает все ветки сортировки эндпойнтов и web-vitals.
func TestCoverSortHelpers(t *testing.T) {
	stats := []trace.EndpointStat{
		{Transaction: "b", Throughput: 1, P50: 2, P75: 3, P95: 4, P99: 5, FailureRate: 0.1, ApdexScore: 0.9},
		{Transaction: "a", Throughput: 2, P50: 1, P75: 2, P95: 3, P99: 4, FailureRate: 0.2, ApdexScore: 0.8},
	}
	for _, key := range []string{"", "name", "p50", "p75", "p95", "p99", "failure", "apdex", "unknown"} {
		cp := append([]trace.EndpointStat(nil), stats...)
		sortEndpointStats(cp, key)
	}

	pages := []trace.PageVitals{
		{Transaction: "b", Count: 1, LCP: trace.Vital{P75: 3}, INP: trace.Vital{P75: 2}, CLS: trace.Vital{P75: 0.1}},
		{Transaction: "a", Count: 2, LCP: trace.Vital{P75: 1}, INP: trace.Vital{P75: 4}, CLS: trace.Vital{P75: 0.2}},
	}
	for _, key := range []string{"", "name", "lcp", "inp", "cls", "unknown"} {
		cp := append([]trace.PageVitals(nil), pages...)
		sortPageVitals(cp, key)
	}
}

// TestCoverIncidentDurationText покрывает ветки: незакрытый (пусто), закрытый с
// часами и минутами, и «отрицательная» длительность (округляется до 0).
func TestCoverIncidentDurationText(t *testing.T) {
	now := time.Now().UTC()
	// Незакрытый → "".
	if got := incidentDurationText(uptime.Incident{StartedAt: now.Add(-time.Hour)}, now); got != "" {
		t.Errorf("ongoing incident duration = %q, want empty", got)
	}
	// Закрытый, 2ч30м → «2h 30m».
	resolved := now
	started := now.Add(-(2*time.Hour + 30*time.Minute))
	if got := incidentDurationText(uptime.Incident{StartedAt: started, ResolvedAt: &resolved}, now); got == "" {
		t.Error("closed incident duration empty")
	}
	// Закрытый только минуты.
	started2 := now.Add(-20 * time.Minute)
	if got := incidentDurationText(uptime.Incident{StartedAt: started2, ResolvedAt: &resolved}, now); got == "" {
		t.Error("closed incident (minutes) duration empty")
	}
	// Отрицательная длительность (resolved до started) → «0m», без паники.
	badStart := now.Add(time.Hour)
	if got := incidentDurationText(uptime.Incident{StartedAt: badStart, ResolvedAt: &resolved}, now); got == "" {
		t.Error("negative duration incident empty")
	}
}

// TestCoverParseHelpers покрывает мелкие парсеры формы монитора и maintenance.
func TestCoverParseHelpers(t *testing.T) {
	if atoiOrZero("42") != 42 || atoiOrZero("nope") != 0 {
		t.Error("atoiOrZero branches")
	}
	// parseHeaderLines: валидная строка, строка без ":", пустой ключ, и пусто→nil.
	h := parseHeaderLines("X-Test: 1\nno-colon-line\n: emptykey\n\nY: 2")
	if h["X-Test"] != "1" || h["Y"] != "2" || len(h) != 2 {
		t.Errorf("parseHeaderLines = %v", h)
	}
	if parseHeaderLines("\n\n") != nil {
		t.Error("parseHeaderLines(empty) should be nil")
	}
	// parseInt64List: валидные и мусорные значения.
	if got := parseInt64List([]string{"1", "x", " 2 "}); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("parseInt64List = %v", got)
	}
	// parseLocalDateTime: пусто, невалидно, валидно.
	if _, ok := parseLocalDateTime("", time.UTC); ok {
		t.Error("parseLocalDateTime(empty) should be false")
	}
	if _, ok := parseLocalDateTime("not-a-date", time.UTC); ok {
		t.Error("parseLocalDateTime(garbage) should be false")
	}
	if _, ok := parseLocalDateTime("2026-07-20T10:30", time.UTC); !ok {
		t.Error("parseLocalDateTime(valid) should be true")
	}
}

// TestCoverParsePerfEvidence покрывает ветки разбора JSONB evidence.
func TestCoverParsePerfEvidence(t *testing.T) {
	if ev := parsePerfEvidence(nil); ev.HasTotal {
		t.Error("empty evidence should have no total")
	}
	if ev := parsePerfEvidence([]byte("{not json")); ev.HasTotal {
		t.Error("invalid JSON evidence should be empty")
	}
	full := []byte(`{"count":5,"total_us":1000,"max_us":800,"parent_op":"db.query","sequential_pct":90,"max_concurrency":3,"urls":["/a","/b"]}`)
	ev := parsePerfEvidence(full)
	if !ev.HasTotal || !ev.HasMax || !ev.HasSequential || ev.Count != 5 || ev.ParentOp != "db.query" || len(ev.URLs) != 2 {
		t.Errorf("parsePerfEvidence full = %+v", ev)
	}
}
