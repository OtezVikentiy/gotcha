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

func TestCoverPathHelpers(t *testing.T) {
	cases := map[string]string{
		orgSettingsRolePath(1):            "/orgs/1/settings/role",
		orgSettingsRemovePath(1):          "/orgs/1/settings/remove",
		orgSettingsInvitePath(1):          "/orgs/1/settings/invite",
		orgSettingsQuotaPath(1):           "/orgs/1/settings/quota",
		orgSettingsPurgeSubjectPath(1):    "/orgs/1/settings/purge-subject",
		orgSettingsExportSubjectPath(1):   "/orgs/1/settings/export-subject",
		teamMembersPath(2):                "/teams/2/members",
		teamMembersRemovePath(2):          "/teams/2/members/remove",
		teamProjectsPath(2):               "/teams/2/projects",
		teamProjectsDetachPath(2):         "/teams/2/projects/detach",
		performancePath(3):                "/projects/3/performance",
		perfIssuesPath(3):                 "/projects/3/perf-issues",
		profilesPath(3):                   "/projects/3/profiles",
		profileRegressionsPath(3):         "/projects/3/profile-regressions",
		regressionsPath(3):                "/projects/3/regressions",
		webVitalsPath(3):                  "/projects/3/web-vitals",
		projectSettingsRenamePath(3):      "/projects/3/settings/rename",
		projectSettingsPerformancePath(3): "/projects/3/settings/performance",
		projectSettingsRegressionsPath(3): "/projects/3/settings/regressions",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path helper = %q, want %q", got, want)
		}
	}
}

func TestCoverErrorMessages(t *testing.T) {
	ctx := context.Background()
	dummy := context.DeadlineExceeded

	orgErrs := []error{org.ErrLastOwner, org.ErrInvalidRole, org.ErrNotMember, org.ErrOwnerOnly, org.ErrInvalidQuota, dummy}
	for _, e := range orgErrs {
		if orgSettingsErrorMessage(ctx, e) == "" {
			t.Errorf("orgSettingsErrorMessage(%v) empty", e)
		}
	}

	for _, e := range []error{uptime.ErrInvalidStatusPage, dummy} {
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

func TestCoverPeriodAndStepHelpers(t *testing.T) {
	if step := perfBucketStep(23*time.Hour, 24); step%(5*time.Minute) != 0 {
		t.Errorf("perfBucketStep not multiple of 5m: %v", step)
	}
	if step := perfBucketStep(time.Minute, 24); step != 5*time.Minute {
		t.Errorf("perfBucketStep(tiny) = %v, want 5m floor", step)
	}
	for _, c := range []struct {
		in   time.Duration
		want string
	}{
		{2 * time.Hour, "2h"},
		{90 * time.Minute, "1h"},
		{30 * time.Minute, "30m"},
		{15 * time.Second, "15s"},
		{0, "0s"},
	} {
		if got := formatStep(c.in); got != c.want {
			t.Errorf("formatStep(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, c := range []struct {
		in   uint32
		want string
	}{
		{500, "500µs"},
		{999, "999µs"},
		{1000, "1.0ms"},
		{5000, "5.0ms"},
		{999_999, "1000.0ms"},
		{5_000_000, "5.00s"},
	} {
		if got := waterfallMS(c.in); got != c.want {
			t.Errorf("waterfallMS(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCoverSortHelpers(t *testing.T) {
	stats := []trace.EndpointStat{
		{Transaction: "b", Throughput: 1, P50: 2, P75: 3, P95: 4, P99: 5, FailureRate: 0.1, ApdexScore: 0.9},
		{Transaction: "a", Throughput: 2, P50: 1, P75: 2, P95: 3, P99: 4, FailureRate: 0.2, ApdexScore: 0.8},
	}
	wantFirst := map[string]string{
		"":        "a",
		"unknown": "a",
		"name":    "a",
		"p50":     "b",
		"p75":     "b",
		"p95":     "b",
		"p99":     "b",
		"failure": "a",
		"apdex":   "a",
	}
	for key, want := range wantFirst {
		cp := append([]trace.EndpointStat(nil), stats...)
		sortEndpointStats(cp, key)
		if cp[0].Transaction != want {
			t.Errorf("sortEndpointStats(%q): первым %q, want %q (порядок: %q, %q)",
				key, cp[0].Transaction, want, cp[0].Transaction, cp[1].Transaction)
		}
	}

	pages := []trace.PageVitals{
		{Transaction: "b", Count: 1, LCP: trace.Vital{P75: 3}, INP: trace.Vital{P75: 2}, CLS: trace.Vital{P75: 0.1}},
		{Transaction: "a", Count: 2, LCP: trace.Vital{P75: 1}, INP: trace.Vital{P75: 4}, CLS: trace.Vital{P75: 0.2}},
	}
	wantFirstVitals := map[string]string{
		"":        "a",
		"unknown": "a",
		"name":    "a",
		"lcp":     "b",
		"inp":     "a",
		"cls":     "a",
	}
	for key, want := range wantFirstVitals {
		cp := append([]trace.PageVitals(nil), pages...)
		sortPageVitals(cp, key)
		if cp[0].Transaction != want {
			t.Errorf("sortPageVitals(%q): первым %q, want %q", key, cp[0].Transaction, want)
		}
	}
}

func TestCoverParseHelpers(t *testing.T) {
	if atoiOrZero("42") != 42 || atoiOrZero("nope") != 0 {
		t.Error("atoiOrZero branches")
	}
	h := parseHeaderLines("X-Test: 1\nno-colon-line\n: emptykey\n\nY: 2")
	if h["X-Test"] != "1" || h["Y"] != "2" || len(h) != 2 {
		t.Errorf("parseHeaderLines = %v", h)
	}
	if parseHeaderLines("\n\n") != nil {
		t.Error("parseHeaderLines(empty) should be nil")
	}
	if got := parseInt64List([]string{"1", "x", " 2 "}); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("parseInt64List = %v", got)
	}
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

func TestValidInviteEmail(t *testing.T) {
	cases := map[string]bool{
		"":                   false,
		"not-an-email":       false,
		"a@b.co":             true,
		"a\x00b@example.com": false,
		"a@ex\x00ample.com":  false,
	}
	for email, want := range cases {
		if got := validInviteEmail(email); got != want {
			t.Errorf("validInviteEmail(%q) = %v, want %v", email, got, want)
		}
	}
}
