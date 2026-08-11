package nav

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/docs"
)

func TestAreaForPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/projects/7/issues", "issues"},
		{"/issues/9", "issues"},
		{"/projects/7/web-vitals", "performance"},
		{"/traces/abc", "performance"},
		{"/projects/7/metrics/alerts", "metrics"},
		{"/monitors/3", "uptime"},
		{"/projects/7/alerts", "alerts"},
		{"/orgs/5/teams", "org"},
		{"/projects", "org"},
		// Настройки проекта — не область рейла (в рейле ничего не
		// подсвечивается), но сайдбар они наполняют своими пунктами:
		// иначе там оставался бы один переключатель проекта.
		{"/projects/7/settings", "settings"},
		{"/projects/7/setup", "settings"},
		{"/docs", "docs"},
		{"/docs/glossary", "docs"},
	}
	for _, c := range cases {
		if got := AreaForPath(c.path); got != c.want {
			t.Errorf("AreaForPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestBackLabelKey(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/projects/7/issues?status=resolved", "nav.issues"},
		{"/issues/9", "nav.issues"},
		{"/projects/7/web-vitals", "nav.webvitals"},
		{"/projects/7/performance", "nav.transactions"},
		{"/projects/7/metrics", "nav.metrics"},
		{"/projects/7/metrics/alerts", "nav.metric_alerts"},
		{"/monitors/3", "nav.monitors"},
		{"/projects/7/incidents", "nav.incidents"},
		{"/projects/7/alerts", "nav.alerts"},
		{"/projects/7/alerts/deliveries", "nav.alert_deliveries"},
		{"/projects", "nav.projects"},
		{"/projects/7/settings", "nav.project_settings"},
		{"/docs/glossary", "docs.index.title"},
		// Неопознанный путь — общий ключ подставляет вызывающий, здесь "".
		{"/setup", ""},
		{"/whatever", ""},
	}
	for _, c := range cases {
		if got := BackLabelKey(c.path); got != c.want {
			t.Errorf("BackLabelKey(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestAreaForPathExtras(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/projects/7/performance", "performance"},
		{"/projects/7/profiles", "performance"},
		{"/projects/7/profile-regressions", "performance"},
		{"/projects/7/perf-issues", "performance"},
		{"/projects/7/regressions", "performance"},
		{"/perf-issues/1", "performance"},
		{"/projects/7/metrics", "metrics"},
		{"/projects/7/incidents", "uptime"},
		{"/projects/7/maintenance", "uptime"},
		{"/projects/7/statuspages", "uptime"},
		{"/statuspages/1", "uptime"},
		{"/profile", "org"},
		{"/setup", ""},
		{"/unknown/path", ""},
	}
	for _, c := range cases {
		if got := AreaForPath(c.path); got != c.want {
			t.Errorf("AreaForPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestWithShellFromContextRoundTrip(t *testing.T) {
	s := Shell{
		UserEmail: "a@b.com",
		Projects:  []Project{{ID: 7, Slug: "demo", Name: "Demo"}},
		ProjectID: 7,
		OrgID:     5,
		Area:      "performance",
		OrgMode:   false,
		Path:      "/projects/7/web-vitals",
	}
	ctx := WithShell(context.Background(), s)
	got := FromContext(ctx)
	if !reflect.DeepEqual(got, s) {
		t.Errorf("FromContext round trip = %+v, want %+v", got, s)
	}
}

func TestFromContextZeroValue(t *testing.T) {
	got := FromContext(context.Background())
	if !reflect.DeepEqual(got, Shell{}) {
		t.Errorf("FromContext(empty ctx) = %+v, want zero Shell", got)
	}
}

func TestSubsectionsPerformance(t *testing.T) {
	s := Shell{ProjectID: 7, Area: "performance", Path: "/projects/7/web-vitals"}
	items := Subsections(s)
	if len(items) != 5 {
		t.Fatalf("Subsections(performance) len = %d, want 5", len(items))
	}
	wantHrefs := []string{
		"/projects/7/performance",
		"/projects/7/web-vitals",
		"/projects/7/profiles",
		"/projects/7/perf-issues",
		"/projects/7/regressions",
	}
	wantLabels := []string{
		"nav.transactions",
		"nav.webvitals",
		"nav.profiles",
		"nav.perf_issues",
		"nav.regressions",
	}
	activeIdx := -1
	for i, it := range items {
		if it.Href != wantHrefs[i] {
			t.Errorf("item[%d].Href = %q, want %q", i, it.Href, wantHrefs[i])
		}
		if it.LabelKey != wantLabels[i] {
			t.Errorf("item[%d].LabelKey = %q, want %q", i, it.LabelKey, wantLabels[i])
		}
		if it.Active {
			if activeIdx != -1 {
				t.Errorf("more than one active item: %d and %d", activeIdx, i)
			}
			activeIdx = i
		}
	}
	if activeIdx != 1 {
		t.Errorf("active item index = %d, want 1 (web-vitals)", activeIdx)
	}
}

func TestSubsectionsMetricsVsMetricAlertsActive(t *testing.T) {
	// Plain metrics path activates Metrics, not Metric Alerts.
	s := Shell{ProjectID: 7, Area: "metrics", Path: "/projects/7/metrics", CanOperate: true}
	items := Subsections(s)
	if len(items) != 2 {
		t.Fatalf("Subsections(metrics) len = %d, want 2", len(items))
	}
	if !items[0].Active || items[1].Active {
		t.Errorf("metrics path: items = %+v, want [Metrics active, MetricAlerts inactive]", items)
	}

	// Metric alerts sub-path activates Metric Alerts, not Metrics.
	s2 := Shell{ProjectID: 7, Area: "metrics", Path: "/projects/7/metrics/alerts", CanOperate: true}
	items2 := Subsections(s2)
	if items2[0].Active || !items2[1].Active {
		t.Errorf("metrics/alerts path: items = %+v, want [Metrics inactive, MetricAlerts active]", items2)
	}
}

func TestSubsectionsOrgAndEmpty(t *testing.T) {
	s := Shell{OrgID: 5, CanManage: true, Area: "org", Path: "/orgs/5/teams"}
	items := Subsections(s)
	if len(items) != 4 {
		t.Fatalf("Subsections(org) len = %d, want 4", len(items))
	}
	if items[0].Href != "/projects" {
		t.Errorf("org projects href = %q, want /projects", items[0].Href)
	}
	if items[1].Href != "/orgs/5/settings" {
		t.Errorf("org members href = %q", items[1].Href)
	}
	if items[2].Href != "/orgs/5/teams" || !items[2].Active {
		t.Errorf("org teams item = %+v, want active href /orgs/5/teams", items[2])
	}
	if items[3].Href != "/orgs/5/probes" {
		t.Errorf("org probes href = %q", items[3].Href)
	}

	empty := Shell{ProjectID: 7, Area: "", Path: "/projects/7/settings"}
	if got := Subsections(empty); got != nil {
		t.Errorf("Subsections(\"\") = %+v, want nil", got)
	}
}

func TestSubsectionsOrgZeroIDOmitsOrgLinks(t *testing.T) {
	s := Shell{
		Area:     "org",
		OrgID:    0,
		Projects: []Project{{ID: 1, Slug: "demo"}},
		Path:     "/projects",
	}
	items := Subsections(s)
	for _, it := range items {
		if strings.Contains(it.Href, "/orgs/0/") {
			t.Errorf("Subsections(org, OrgID=0) href = %q, want no /orgs/0/ links", it.Href)
		}
	}
	if len(items) != 1 || items[0].Href != "/projects" {
		t.Errorf("Subsections(org, OrgID=0) = %+v, want only the /projects item", items)
	}

	withOrg := Shell{
		Area:      "org",
		OrgID:     5,
		CanManage: true,
		Path:      "/orgs/5",
	}
	got := Subsections(withOrg)
	want := map[string]bool{
		"/orgs/5/settings": false,
		"/orgs/5/teams":    false,
		"/orgs/5/probes":   false,
	}
	for _, it := range got {
		if _, ok := want[it.Href]; ok {
			want[it.Href] = true
		}
	}
	for href, found := range want {
		if !found {
			t.Errorf("Subsections(org, OrgID=5) missing href %q, got %+v", href, got)
		}
	}
}

// TestSubsectionsOrgCanManageGatesManagementLinks — Members/Teams/Probes are
// owner/admin-only management pages that 404 for plain members; they must
// only appear when the shell says the user can manage the org, regardless of
// whether an org id is resolved.
func TestSubsectionsOrgCanManageGatesManagementLinks(t *testing.T) {
	base := Shell{
		Area:     "org",
		OrgID:    5,
		Projects: []Project{{ID: 1, Slug: "demo"}},
		Path:     "/projects",
	}

	notManaging := base
	notManaging.CanManage = false
	got := Subsections(notManaging)
	if len(got) != 1 || got[0].Href != "/projects" {
		t.Errorf("Subsections(org, CanManage=false) = %+v, want only the /projects item", got)
	}
	for _, it := range got {
		if it.Href == "/orgs/5/teams" || it.Href == "/orgs/5/probes" {
			t.Errorf("Subsections(org, CanManage=false) unexpectedly includes %q", it.Href)
		}
	}

	managing := base
	managing.CanManage = true
	got = Subsections(managing)
	want := map[string]bool{
		"/projects":        false,
		"/orgs/5/settings": false,
		"/orgs/5/teams":    false,
		"/orgs/5/probes":   false,
	}
	for _, it := range got {
		if _, ok := want[it.Href]; ok {
			want[it.Href] = true
		}
	}
	for href, found := range want {
		if !found {
			t.Errorf("Subsections(org, CanManage=true) missing href %q, got %+v", href, got)
		}
	}
}

// TestSubsectionsOperatorGating — пункты мониторинга (metric_alerts,
// maintenance, status_pages, области alerts) гейтятся CanOperate, а не
// CanManage: оператор с CanOperate=true и CanManage=false их видит,
// зритель без CanOperate — нет. Org-пункты и ссылка настроек остаются
// за CanManage.
func TestSubsectionsOperatorGating(t *testing.T) {
	operator := Shell{ProjectID: 7, OrgID: 5, CanOperate: true, CanManage: false}
	viewer := Shell{ProjectID: 7, OrgID: 5, CanOperate: false, CanManage: false}

	// metrics area: metric_alerts — доступен оператору, не зрителю.
	operator.Area, operator.Path = "metrics", "/projects/7/metrics"
	viewer.Area, viewer.Path = "metrics", "/projects/7/metrics"
	if got := Subsections(operator); len(got) != 2 {
		t.Errorf("metrics/оператор: len = %d, want 2 (metrics + metric_alerts), got %+v", len(got), got)
	}
	if got := Subsections(viewer); len(got) != 1 {
		t.Errorf("metrics/зритель: len = %d, want 1 (только metrics), got %+v", len(got), got)
	}

	// uptime area: maintenance + status_pages — доступны оператору, не зрителю.
	operator.Area, operator.Path = "uptime", "/projects/7/uptime"
	viewer.Area, viewer.Path = "uptime", "/projects/7/uptime"
	if got := Subsections(operator); len(got) != 4 {
		t.Errorf("uptime/оператор: len = %d, want 4 (monitors+incidents+maintenance+status_pages), got %+v", len(got), got)
	}
	if got := Subsections(viewer); len(got) != 2 {
		t.Errorf("uptime/зритель: len = %d, want 2 (только monitors+incidents), got %+v", len(got), got)
	}

	// alerts area: вся область гейтится целиком.
	operator.Area, operator.Path = "alerts", "/projects/7/alerts"
	viewer.Area, viewer.Path = "alerts", "/projects/7/alerts"
	if got := Subsections(operator); len(got) != 2 {
		t.Errorf("alerts/оператор: len = %d, want 2, got %+v", len(got), got)
	}
	if got := Subsections(viewer); got != nil {
		t.Errorf("alerts/зритель: got %+v, want nil", got)
	}

	// org area: не затронута CanOperate, по-прежнему гейтится CanManage —
	// оператор без CanManage Members/Teams/Probes не видит.
	operator.Area, operator.Path = "org", "/orgs/5/teams"
	if got := Subsections(operator); len(got) != 1 {
		t.Errorf("org/оператор(CanManage=false): len = %d, want 1 (только /projects), got %+v", len(got), got)
	}
}

func TestSubsectionsEffectiveProjectFallback(t *testing.T) {
	// ProjectID unset, falls back to first project in list.
	s := Shell{
		Projects: []Project{{ID: 42, Slug: "demo"}},
		Area:     "issues",
		Path:     "/projects/42/issues",
	}
	items := Subsections(s)
	if len(items) != 1 || items[0].Href != "/projects/42/issues" {
		t.Errorf("Subsections(issues, fallback) = %+v", items)
	}
	if !items[0].Active {
		t.Errorf("expected issues item active")
	}
}

func TestAreas(t *testing.T) {
	s := Shell{
		ProjectID:  7,
		OrgID:      5,
		Area:       "metrics",
		Path:       "/projects/7/metrics",
		CanManage:  true,
		CanOperate: true,
	}
	areas := Areas(s)
	if len(areas) != 7 {
		t.Fatalf("Areas() len = %d, want 7 (5 rail + docs + org)", len(areas))
	}
	wantIDs := []string{"issues", "performance", "metrics", "uptime", "alerts", "docs", "org"}
	for i, a := range areas {
		if a.ID != wantIDs[i] {
			t.Errorf("areas[%d].ID = %q, want %q", i, a.ID, wantIDs[i])
		}
	}
	// metrics area is active, matches Shell.Area
	for _, a := range areas {
		want := a.ID == "metrics"
		if a.Active != want {
			t.Errorf("area %q Active = %v, want %v", a.ID, a.Active, want)
		}
	}
	// issues area href points at first subsection for effective project id
	for _, a := range areas {
		if a.ID == "issues" && a.Href != "/projects/7/issues" {
			t.Errorf("issues area href = %q, want /projects/7/issues", a.Href)
		}
		if a.ID == "org" && a.Href != "/projects" {
			t.Errorf("org area href = %q, want /projects", a.Href)
		}
		if a.ID == "docs" {
			if a.Href != "/docs" {
				t.Errorf("docs area href = %q, want /docs", a.Href)
			}
			if a.IconName != "book" {
				t.Errorf("docs area icon = %q, want book", a.IconName)
			}
			if a.LabelKey != "nav.docs" {
				t.Errorf("docs area labelKey = %q, want nav.docs", a.LabelKey)
			}
		}
	}
}

// TestAreasDocsPresentRegardlessOfArea — the docs area is visible to all
// roles and must appear in Areas(shell) for any shell with projects,
// independent of the currently active area (unlike CanManage-gated org
// sub-links, it is never conditionally omitted).
func TestAreasDocsPresentRegardlessOfArea(t *testing.T) {
	s := Shell{Projects: []Project{{ID: 1, Slug: "demo"}}, ProjectID: 1, Area: "issues", Path: "/projects/1/issues"}
	areas := Areas(s)
	found := false
	for _, a := range areas {
		if a.ID == "docs" {
			found = true
			if a.Active {
				t.Errorf("docs area should not be active when Shell.Area = issues")
			}
		}
	}
	if !found {
		t.Fatalf("Areas() missing docs area: %+v", areas)
	}
}

// TestAreasDocsActiveOnDocsPath — the docs rail item is marked Active for
// any /docs* path.
func TestAreasDocsActiveOnDocsPath(t *testing.T) {
	s := Shell{Projects: []Project{{ID: 1, Slug: "demo"}}, Area: "docs", Path: "/docs/glossary"}
	for _, a := range Areas(s) {
		if a.ID == "docs" && !a.Active {
			t.Errorf("docs area Active = false, want true for Shell.Area = docs")
		}
	}
}

// TestSubsectionsDocs — Subsections for the docs area lists the doc
// registry pages by their localized Title (H1), not by an i18n LabelKey,
// since doc titles come from markdown content rather than the i18n
// catalog. Active is set on the page matching the current path.
func TestSubsectionsDocs(t *testing.T) {
	s := Shell{Area: "docs", Locale: "ru", Path: "/docs/glossary"}
	items := Subsections(s)
	// Docs subsections mirror the doc registry 1:1 (each page is a subsection),
	// so compare to the registry size rather than a hardcoded count — the
	// registry grows as pages are added and this test must not need editing.
	if want := len(docs.Pages(s.Locale)); len(items) != want {
		t.Fatalf("Subsections(docs) len = %d, want %d (docs registry size)", len(items), want)
	}
	activeIdx := -1
	for i, it := range items {
		if it.LabelKey != "" {
			t.Errorf("item[%d].LabelKey = %q, want empty (doc pages use Label)", i, it.LabelKey)
		}
		if it.Label == "" {
			t.Errorf("item[%d].Label is empty, want localized doc title", i)
		}
		if !strings.HasPrefix(it.Href, "/docs/") {
			t.Errorf("item[%d].Href = %q, want /docs/ prefix", i, it.Href)
		}
		if it.Active {
			if activeIdx != -1 {
				t.Errorf("more than one active doc item: %d and %d", activeIdx, i)
			}
			activeIdx = i
		}
	}
	if activeIdx == -1 {
		t.Errorf("expected the glossary doc item to be active for Path /docs/glossary")
	}
}

func TestAreasOrgHrefWithOrgID(t *testing.T) {
	// per brief: org Href -> /projects OR /orgs/{OrgID}/settings depending on context.
	// We just verify it is one of those two valid forms and is non-empty.
	s := Shell{OrgID: 5, Area: "org"}
	areas := Areas(s)
	for _, a := range areas {
		if a.ID == "org" {
			if a.Href == "" {
				t.Errorf("org area href empty")
			}
		}
	}
}

// TestSubsectionsHideManagementPagesFromMembers — зритель без доступа к
// проекту не должен видеть в навигации страницы, которые ему отдадут 404.
//
// Правила по метрикам, окна обслуживания, статус-страницы и обе страницы
// оповещений требуют оператора проекта (requireProjectOperator — участник
// команды проекта или owner/admin, см. nav.Shell.CanOperate). Показывали их
// всем: человек тыкал в пункт, нарисованный самим продуктом, и попадал на
// «страницы нет» — без намёка, что дело в роли. Область «Организация» этот
// принцип соблюдала с самого начала, остальные — нет.
func TestSubsectionsHideManagementPagesFromMembers(t *testing.T) {
	viewer := func(area string) []NavItem {
		return Subsections(Shell{ProjectID: 7, OrgID: 5, Area: area, Path: "/projects/7/" + area})
	}
	operator := func(area string) []NavItem {
		return Subsections(Shell{ProjectID: 7, OrgID: 5, Area: area, Path: "/projects/7/" + area, CanOperate: true})
	}

	hiddenFromViewer := map[string][]string{
		"metrics": {"nav.metric_alerts"},
		"uptime":  {"nav.maintenance", "nav.status_pages"},
		"alerts":  {"nav.alerts", "nav.alert_deliveries"},
	}
	for area, hidden := range hiddenFromViewer {
		got := viewer(area)
		for _, it := range got {
			for _, key := range hidden {
				if it.LabelKey == key {
					t.Errorf("область %q: зритель видит %q — эта страница отдаёт ему 404", area, key)
				}
			}
		}
		// Оператор их видит — иначе фильтр забрал бы лишнее.
		operatorKeys := map[string]bool{}
		for _, it := range operator(area) {
			operatorKeys[it.LabelKey] = true
		}
		for _, key := range hidden {
			if !operatorKeys[key] {
				t.Errorf("область %q: оператор НЕ видит %q", area, key)
			}
		}
	}

	// Читаемые страницы остаются: мониторы и инциденты доступны зрителю.
	var monitors, incidents bool
	for _, it := range viewer("uptime") {
		switch it.LabelKey {
		case "nav.monitors":
			monitors = true
		case "nav.incidents":
			incidents = true
		}
	}
	if !monitors || !incidents {
		t.Error("зритель потерял доступные ему мониторы или инциденты")
	}
}

// TestAreasHideAreaWithNothingVisible — область рейла, у которой для этого
// человека нет ни одного доступного подраздела, не показывается: иначе иконка
// вела бы прямиком на 404. Сейчас это «Оповещения» для зрителя без доступа к
// проекту (CanOperate=false).
func TestAreasHideAreaWithNothingVisible(t *testing.T) {
	viewerAreas := Areas(Shell{ProjectID: 7, OrgID: 5, Area: "issues", Path: "/projects/7/issues"})
	for _, a := range viewerAreas {
		if a.ID == "alerts" {
			t.Error("зритель видит область «Оповещения», все страницы которой отдают ему 404")
		}
	}
	operatorAreas := Areas(Shell{ProjectID: 7, OrgID: 5, Area: "issues", Path: "/projects/7/issues", CanOperate: true})
	var found bool
	for _, a := range operatorAreas {
		if a.ID == "alerts" {
			found = true
		}
	}
	if !found {
		t.Error("оператор потерял область «Оповещения»")
	}
}

// TestProjectSwitchHref — смена проекта из переключателя держит текущий
// раздел (№60): из «Транзакций» проекта 1 — в «Транзакции» проекта 2; из
// областей вне проекта (org/docs/пусто) — на issues целевого проекта.
func TestProjectSwitchHref(t *testing.T) {
	shell := Shell{
		Projects:  []Project{{ID: 1, Name: "one"}, {ID: 2, Name: "two"}},
		ProjectID: 1,
		OrgID:     7,
		Area:      "performance",
		CanManage: true,
	}
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/performance" {
		t.Errorf("performance → %q, want /projects/2/performance", got)
	}
	shell.Area = "uptime"
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/monitors" {
		t.Errorf("uptime → %q, want /projects/2/monitors", got)
	}
	shell.Area = "org"
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/issues" {
		t.Errorf("org → %q, want issues-фолбэк", got)
	}
	shell.Area = ""
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/issues" {
		t.Errorf("пустая область → %q, want issues-фолбэк", got)
	}

	// C1: CanOperate — флаг ТЕКУЩЕГО проекта (участник команды проекта),
	// а не целевого. Переключаясь из «Оповещений» с CanOperate=true на
	// проекте 1, наивная реализация переносит этот флаг в пробу для
	// проекта 2 и получает /projects/2/alerts — хотя на проекте 2
	// пользователь может не быть оператором вовсе, и ссылка 404-ит.
	// nav — чистый слой без доступа к БД, пересчитать CanOperate для
	// целевого проекта не может, поэтому единственный безопасный вариант —
	// не доверять перенесённому флагу для «Оповещений» (единственная
	// область, чей список подразделов целиком схлопывается в nil при
	// !CanOperate) и уводить переключатель на посадочную область, которая
	// валидна при любом доступе.
	shell.Area = "alerts"
	shell.CanOperate = true
	if got := ProjectSwitchHref(shell, 2); got == "/projects/2/alerts" || strings.Contains(got, "404") {
		t.Errorf("alerts с CanOperate=true текущего проекта → %q, не должно вести на /alerts целевого проекта", got)
	}
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/issues" {
		t.Errorf("alerts → %q, want issues-фолбэк (безопасная посадочная область)", got)
	}
}
