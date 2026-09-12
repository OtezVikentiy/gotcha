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
		{"/projects/7/exports", "issues"},
		{"/projects/7/perf-issues", "issues"},
		{"/projects/7/regressions", "issues"},
		{"/issues/9", "issues"},
		{"/projects/7/web-vitals", "performance"},
		{"/projects/7/profile-regressions", "performance"},
		{"/traces/abc", "performance"},
		{"/projects/7/metrics/alerts", "alerts"},
		{"/projects/7/recipes", "settings"},
		{"/projects/7/recipes/redis", "settings"},
		{"/projects/5/hosts", "hosts"},
		{"/projects/5/hosts/settings", "alerts"},
		{"/projects/5/hosts/web-1", "hosts"},
		{"/projects/5/logs", "logs"},
		{"/monitors/3", "uptime"},
		{"/projects/7/alerts", "alerts"},
		{"/projects/7/slos", "alerts"},
		{"/projects/7/escalations", "alerts"},
		{"/projects/7/maintenance", "alerts"},
		{"/projects/7/statuspages", "settings"},
		{"/orgs/5/settings", "settings"},
		{"/orgs/5/teams", "settings"},
		{"/orgs/5/probes", "settings"},
		{"/orgs/5/projects", ""},
		{"/profile", ""},
		{"/projects", ""},
		// Настройки — не область рейла, но наполняют сайдбар: без них там был бы
		// только переключатель проекта.
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
		{"/projects/7/overview", "nav.overview"},
		{"/projects/7/overview?range=7d", "nav.overview"},
		{"/projects/7/incident-feed", "nav.overview"},
		{"/projects/7/issues?status=resolved", "nav.errors"},
		{"/projects/7/exports", "nav.exports"},
		{"/issues/9", "nav.issues"},
		{"/projects/7/web-vitals", "nav.webvitals"},
		{"/projects/7/dependencies", "nav.dependencies"},
		{"/projects/7/performance", "nav.transactions"},
		{"/projects/7/deployments", "nav.deployments"},
		{"/projects/7/metrics", "nav.metrics"},
		{"/projects/7/metrics/alerts", "nav.metric_alerts"},
		{"/projects/7/recipes", "nav.recipes"},
		{"/projects/7/recipes/redis", "nav.recipes"},
		{"/projects/5/hosts", "nav.hosts"},
		{"/projects/5/hosts/web-1", "nav.hosts"},
		{"/projects/5/hosts/settings", "nav.host_thresholds"},
		{"/projects/5/logs", "nav.logs"},
		{"/monitors/3", "nav.monitors"},
		{"/projects/7/incidents", "nav.incidents"},
		{"/projects/7/alerts", "nav.rules_errors"},
		{"/projects/7/alerts/deliveries", "nav.alert_deliveries"},
		{"/projects/7/slos", "nav.slo"},
		{"/projects/7/escalations", "nav.escalations"},
		{"/projects", ""},
		{"/projects/7/settings", "nav.project_settings"},
		{"/docs/glossary", "docs.index.title"},
		{"/orgs/5/settings", "nav.members"},
		{"/orgs/5/teams", "nav.teams"},
		{"/orgs/5/probes", "nav.probes"},
		{"/orgs/5/projects", "nav.projects"},
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
		{"/projects/7/perf-issues", "issues"},
		{"/projects/7/regressions", "issues"},
		{"/projects/7/dependencies", "performance"},
		{"/projects/7/deployments", "performance"},
		{"/perf-issues/1", "issues"},
		{"/projects/7/metrics", "metrics"},
		{"/projects/7/overview", "overview"},
		{"/projects/7/incident-feed", "overview"},
		{"/projects/7/incidents", "uptime"},
		{"/projects/7/maintenance", "alerts"},
		{"/projects/7/statuspages", "settings"},
		{"/statuspages/1", "uptime"},
		{"/profile", ""},
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
		"/projects/7/dependencies",
		"/projects/7/deployments",
	}
	wantLabels := []string{
		"nav.transactions",
		"nav.webvitals",
		"nav.profiles",
		"nav.dependencies",
		"nav.deployments",
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

func TestSubsectionsUnknownAreaIsNil(t *testing.T) {
	s := Shell{ProjectID: 7, Area: "", Path: "/projects/7/settings"}
	if got := Subsections(s); got != nil {
		t.Errorf("Subsections(\"\") = %+v, want nil", got)
	}
}

func TestSubsectionsSettingsOrgGroupGating(t *testing.T) {
	hasOrgGroup := func(items []NavItem) bool {
		for _, it := range items {
			if it.Group == "nav.group.org" {
				return true
			}
		}
		return false
	}

	zeroOrg := Shell{ProjectID: 7, OrgID: 0, CanManage: true, Area: "settings", Path: "/projects/7/settings"}
	if got := Subsections(zeroOrg); hasOrgGroup(got) {
		t.Errorf("Subsections(settings, OrgID=0, CanManage=true) содержит группу «Организация»: %+v", got)
	}

	notManaging := Shell{ProjectID: 7, OrgID: 5, CanManage: false, Area: "settings", Path: "/projects/7/settings"}
	if got := Subsections(notManaging); hasOrgGroup(got) {
		t.Errorf("Subsections(settings, CanManage=false) содержит группу «Организация»: %+v", got)
	}

	managing := Shell{ProjectID: 7, OrgID: 5, CanManage: true, Area: "settings", Path: "/projects/7/settings"}
	if got := Subsections(managing); !hasOrgGroup(got) {
		t.Errorf("Subsections(settings, OrgID=5, CanManage=true) не содержит группу «Организация»: %+v", got)
	}
}

func TestSubsectionsHideManagementPagesFromMembers(t *testing.T) {
	viewer := Shell{ProjectID: 7, OrgID: 5, Area: "settings", Path: "/projects/7/settings"}
	got := Subsections(viewer)
	for _, it := range got {
		if it.LabelKey == "nav.project_settings" || it.LabelKey == "nav.status_pages" {
			t.Errorf("зритель видит %q — эта страница отдаёт ему 404: %+v", it.LabelKey, got)
		}
	}

	owner := viewer
	owner.CanManage = true
	owner.CanOperate = true
	got = Subsections(owner)
	keys := map[string]bool{}
	for _, it := range got {
		keys[it.LabelKey] = true
	}
	for _, key := range []string{"nav.project_settings", "nav.status_pages"} {
		if !keys[key] {
			t.Errorf("владелец НЕ видит %q: %+v", key, got)
		}
	}

	// Читаемые страницы остаются: мониторы и инциденты доступны зрителю.
	var monitors, incidents bool
	for _, it := range Subsections(Shell{ProjectID: 7, OrgID: 5, Area: "uptime", Path: "/projects/7/uptime"}) {
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

func TestSubsectionsIssuesExportsGatedByCanOperate(t *testing.T) {
	base := Shell{ProjectID: 7, Area: "issues", Path: "/projects/7/issues", ExportsEnabled: true}

	viewer := Subsections(base)
	if len(viewer) != 3 {
		t.Errorf("зритель: len(Subsections) = %d, want 3 (errors+perf_issues+regressions, без exports), got %+v", len(viewer), viewer)
	}

	base.CanOperate = true
	operator := Subsections(base)
	if len(operator) != 4 {
		t.Fatalf("оператор: len(Subsections) = %d, want 4 (+exports), got %+v", len(operator), operator)
	}
	if operator[3].LabelKey != "nav.exports" || operator[3].Href != "/projects/7/exports" {
		t.Errorf("оператор: exports item = %+v", operator[3])
	}

	base.ExportsEnabled = false
	disabled := Subsections(base)
	if len(disabled) != 3 {
		t.Errorf("оператор без ExportsEnabled: len(Subsections) = %d, want 3 (без exports), got %+v", len(disabled), disabled)
	}
}

func TestSubsectionsPerformanceProfileRegressionsGatedByFlag(t *testing.T) {
	base := Shell{ProjectID: 7, Area: "performance", Path: "/projects/7/performance"}

	without := Subsections(base)
	if len(without) != 5 {
		t.Fatalf("без ProfileRegressionsEnabled: len(Subsections) = %d, want 5, got %+v", len(without), without)
	}

	base.ProfileRegressionsEnabled = true
	with := Subsections(base)
	if len(with) != 6 {
		t.Fatalf("с ProfileRegressionsEnabled: len(Subsections) = %d, want 6, got %+v", len(with), with)
	}
	last := with[5]
	if last.LabelKey != "nav.profile_regressions" || last.Href != "/projects/7/profile-regressions" {
		t.Errorf("пункт «регрессии профилей» = %+v", last)
	}

	// Подсветка активного пункта на его собственной странице.
	base.Path = "/projects/7/profile-regressions"
	active := Subsections(base)
	activeIdx := -1
	for i, it := range active {
		if it.Active {
			activeIdx = i
		}
	}
	if activeIdx != 5 {
		t.Errorf("на /profile-regressions активный индекс = %d, want 5, got %+v", activeIdx, active)
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
	if len(items) != 3 || items[0].Href != "/projects/42/issues" {
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
	if len(areas) != 10 {
		t.Fatalf("Areas() len = %d, want 10 (overview + 8 rail incl. settings + docs)", len(areas))
	}
	wantIDs := []string{"overview", "issues", "performance", "logs", "metrics", "hosts", "uptime", "alerts", "settings", "docs"}
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

func TestAreasOrderAndTiers(t *testing.T) {
	s := Shell{Projects: []Project{{ID: 7}}, ProjectID: 7, OrgID: 3, CanOperate: true, CanManage: true}
	var got []string
	for _, a := range Areas(s) {
		got = append(got, a.ID)
	}
	want := []string{
		"overview", "issues", "performance", "logs", "metrics", "hosts", "uptime",
		"alerts", "settings", "docs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Areas order = %v, want %v", got, want)
	}
	for _, a := range Areas(s) {
		if a.ID == "settings" || a.ID == "docs" {
			if !a.Footer {
				t.Errorf("area %q must live in the rail footer", a.ID)
			}
			continue
		}
		if a.Footer {
			t.Errorf("area %q must not live in the rail footer", a.ID)
		}
	}
}

func TestAreasIncludeOverviewFirst(t *testing.T) {
	s := Shell{Projects: []Project{{ID: 7}}, ProjectID: 7, OrgID: 3}
	areas := Areas(s)
	if len(areas) == 0 || areas[0].ID != "overview" {
		t.Fatalf("первая область рейла = %+v, want overview", areas)
	}
	if areas[0].Href != "/projects/7/overview" {
		t.Fatalf("href обзора = %q, want /projects/7/overview", areas[0].Href)
	}
	if subs := Subsections(Shell{Area: "overview", ProjectID: 7}); subs != nil {
		t.Fatalf("у «Обзора» не должно быть подразделов, получено %v", subs)
	}
}

func TestAreasHideAlertsForPlainMember(t *testing.T) {
	s := Shell{Projects: []Project{{ID: 7}}, ProjectID: 7, OrgID: 3}
	for _, a := range Areas(s) {
		if a.ID == "alerts" {
			t.Fatalf("«Оповещения» не должны показываться участнику без CanOperate: все подразделы области закрыты, иконка вела бы на 404")
		}
	}
}

func TestAreaForOrigin(t *testing.T) {
	cases := []struct{ origin, want string }{
		{"web-vitals", "performance"},
		{"endpoint", "performance"},
		{"issue", "issues"},
		{"perf-issue", "issues"},
		{"unknown", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := AreaForOrigin(c.origin); got != c.want {
			t.Errorf("AreaForOrigin(%q) = %q, want %q", c.origin, got, c.want)
		}
	}
}

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

func TestAreasDocsActiveOnDocsPath(t *testing.T) {
	s := Shell{Projects: []Project{{ID: 1, Slug: "demo"}}, Area: "docs", Path: "/docs/glossary"}
	for _, a := range Areas(s) {
		if a.ID == "docs" && !a.Active {
			t.Errorf("docs area Active = false, want true for Shell.Area = docs")
		}
	}
}

func TestSubsectionsDocs(t *testing.T) {
	s := Shell{Area: "docs", Locale: "ru", Path: "/docs/glossary"}
	items := Subsections(s)
	// Сравниваем с размером реестра, не с хардкодным числом — тест не должен
	// править при росте реестра.
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

func TestAreasHideAreaWithNothingVisible(t *testing.T) {
	viewerAreas := Areas(Shell{ProjectID: 7, OrgID: 5, Area: "issues", Path: "/projects/7/issues"})
	for _, a := range viewerAreas {
		if a.ID == "alerts" {
			t.Errorf("зритель не должен видеть область «Оповещения»: %+v", a)
		}
	}

	operatorAreas := Areas(Shell{ProjectID: 7, OrgID: 5, Area: "issues", Path: "/projects/7/issues", CanOperate: true})
	var found bool
	var operatorHref string
	for _, a := range operatorAreas {
		if a.ID == "alerts" {
			found = true
			operatorHref = a.Href
		}
	}
	if !found {
		t.Error("оператор потерял область «Оповещения»")
	}
	if operatorHref != "/projects/7/alerts" {
		t.Errorf("оператор: alerts.Href = %q, want /projects/7/alerts", operatorHref)
	}
}

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
	// hosts: Subsections непустой при любом CanOperate — переключатель может
	// остаться в разделе вместо issues-фолбэка.
	shell.Area = "hosts"
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/hosts" {
		t.Errorf("hosts → %q, want /projects/2/hosts", got)
	}
	// logs: та же логика — единственный подраздел открыт всем с доступом к проекту.
	shell.Area = "logs"
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/logs" {
		t.Errorf("logs → %q, want /projects/2/logs", got)
	}
	shell.Area = "uptime"
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/monitors" {
		t.Errorf("uptime → %q, want /projects/2/monitors", got)
	}
	shell.Area = "org"
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/overview" {
		t.Errorf("org → %q, want overview-фолбэк", got)
	}
	shell.Area = ""
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/overview" {
		t.Errorf("пустая область → %q, want overview-фолбэк", got)
	}

	// CanOperate текущего проекта не переносится на целевой — alerts целиком
	// им гейтится, поэтому переключатель падает в overview-фолбэк.
	shell.Area = "alerts"
	shell.CanOperate = true
	if got := ProjectSwitchHref(shell, 2); got != "/projects/2/overview" {
		t.Errorf("alerts → %q, want overview-фолбэк (CanOperate целевого проекта не подтверждён)", got)
	}
}

func TestProjectSwitchHrefDoesNotTrustCanOperateAcrossProjects(t *testing.T) {
	shell := Shell{
		Projects:   []Project{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}},
		ProjectID:  1,
		OrgID:      7,
		Area:       "alerts",
		CanOperate: true, // оператор проекта A (текущего)
	}
	got := ProjectSwitchHref(shell, 2)
	if got == "/projects/2/alerts" {
		t.Fatalf("ProjectSwitchHref(A→B) = %q, ведёт на страницу, требующую CanOperate целевого проекта — небезопасно", got)
	}
	if got != "/projects/2/overview" {
		t.Errorf("ProjectSwitchHref(A→B) = %q, want /projects/2/overview (заведомо доступный путь)", got)
	}
}

func TestGroupedSubsectionsSkipsEmptyGroups(t *testing.T) {
	items := []NavItem{
		{LabelKey: "a", Href: "/a"},
		{LabelKey: "b", Href: "/b", Group: "nav.group.rules"},
		{LabelKey: "c", Href: "/c", Group: "nav.group.rules"},
		{LabelKey: "d", Href: "/d", Group: "nav.group.delivery"},
	}
	got := groupItems(items)
	want := []NavGroup{
		{LabelKey: "", Items: items[0:1]},
		{LabelKey: "nav.group.rules", Items: items[1:3]},
		{LabelKey: "nav.group.delivery", Items: items[3:4]},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groupItems() = %#v, want %#v", got, want)
	}
	if got := groupItems(items[0:1]); len(got) != 1 || got[0].LabelKey != "" {
		t.Fatalf("groupItems(single ungrouped) = %#v, want one headerless group", got)
	}
}

func TestSubsectionsTargetLayout(t *testing.T) {
	base := Shell{Projects: []Project{{ID: 7}}, ProjectID: 7, OrgID: 3}

	operator := base
	operator.CanOperate = true
	admin := operator
	admin.CanManage = true

	cases := []struct {
		name string
		s    Shell
		area string
		want []string // пары "ключ_группы|ключ_пункта"
	}{
		{"проблемы: участник", base, "issues", []string{
			"|nav.errors", "|nav.perf_issues", "|nav.regressions",
		}},
		{"аптайм: инциденты переименованы", base, "uptime", []string{
			"|nav.monitors", "|nav.incidents",
		}},
		{"аптайм: оператор — по-прежнему без роста", operator, "uptime", []string{
			"|nav.monitors", "|nav.incidents",
		}},
		{"оповещения: участник не видит ничего", base, "alerts", nil},
		{"оповещения: оператор видит три группы", operator, "alerts", []string{
			"nav.group.rules|nav.rules_errors",
			"nav.group.rules|nav.metric_alerts",
			"nav.group.rules|nav.host_thresholds",
			"nav.group.rules|nav.slo",
			"nav.group.silence|nav.maintenance",
			"nav.group.silence|nav.alert_suppression",
			"nav.group.delivery|nav.escalations",
			"nav.group.delivery|nav.alert_deliveries",
		}},
		{"настройки: участник видит только проектную часть", base, "settings", []string{
			"nav.group.project|nav.recipes", "nav.group.project|getting_started.title",
		}},
		{"настройки: оператор без CanManage видит статус-страницы, но не проектные настройки и не организацию", operator, "settings", []string{
			"nav.group.project|nav.status_pages",
			"nav.group.project|nav.recipes",
			"nav.group.project|getting_started.title",
		}},
		{"настройки: владелец видит обе группы", admin, "settings", []string{
			"nav.group.project|nav.project_settings",
			"nav.group.project|nav.status_pages",
			"nav.group.project|nav.recipes",
			"nav.group.project|getting_started.title",
			"nav.group.org|nav.members",
			"nav.group.org|nav.teams",
			"nav.group.org|nav.probes",
		}},
		{"метрики: одна страница", base, "metrics", []string{"|nav.metrics"}},
		{"метрики: оператор — по-прежнему без роста", operator, "metrics", []string{"|nav.metrics"}},
		{"хосты: одна страница", base, "hosts", []string{"|nav.hosts"}},
		{"хосты: оператор — по-прежнему без роста", operator, "hosts", []string{"|nav.hosts"}},
		{"организация упразднена", admin, "org", nil},
	}

	for _, c := range cases {
		s := c.s
		s.Area = c.area
		var got []string
		for _, it := range Subsections(s) {
			key := it.LabelKey
			if key == "" {
				key = it.Label
			}
			got = append(got, it.Group+"|"+key)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Subsections = %v, want %v", c.name, got, c.want)
		}
	}
}
