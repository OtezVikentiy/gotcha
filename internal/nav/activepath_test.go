package nav

import "testing"

// TestSubsectionsHighlightDetailPages — на страницах-деталях подсвечивается
// подраздел их списка. Детали живут на корневых адресах без идентификатора
// проекта (/issues/{id} и т.п.), а пункты сайдбара — на /projects/{id}/…,
// поэтому раньше на детали не подсвечивалось ничего и было не понять, в каком
// разделе находишься.
func TestSubsectionsHighlightDetailPages(t *testing.T) {
	cases := []struct {
		path string
		area string
		want string // LabelKey активного пункта
	}{
		{"/issues/42", "issues", "nav.errors"},
		{"/perf-issues/218", "issues", "nav.perf_issues"},
		{"/traces/abc123", "performance", "nav.transactions"},
		{"/monitors/7", "uptime", "nav.monitors"},
		// Обычные пути продолжают работать как раньше.
		{"/projects/5/web-vitals", "performance", "nav.webvitals"},
		{"/projects/5/metrics/alerts", "alerts", "nav.metric_alerts"},
		// Управление организацией (I2, nav-ia): /orgs/{id}/settings|teams|
		// probes подсвечивают свой пункт в группе «Организация» области
		// «Настройки» — не проваливаются в пустой сайдбар.
		{"/orgs/7/settings", "settings", "nav.members"},
		{"/orgs/7/teams", "settings", "nav.teams"},
		{"/orgs/7/probes", "settings", "nav.probes"},
	}
	for _, c := range cases {
		// CanManage/CanOperate: подразделы фильтруются по роли (по обоим
		// скоупам), а проверяется здесь подсветка — она должна работать для
		// того, кто эти пункты видит.
		items := Subsections(Shell{ProjectID: 5, OrgID: 7, Area: c.area, Path: c.path, CanManage: true, CanOperate: true})
		var active string
		for _, it := range items {
			if it.Active {
				active = it.LabelKey
			}
		}
		if active != c.want {
			t.Errorf("путь %q: подсвечен %q, want %q", c.path, active, c.want)
		}
	}
}
