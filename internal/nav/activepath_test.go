package nav

import "testing"

func TestSubsectionsHighlightDetailPages(t *testing.T) {
	cases := []struct {
		path string
		area string
		want string // LabelKey активного пункта
	}{
		{"/issues/42", "issues", "nav.issues"},
		{"/perf-issues/218", "issues", "nav.perf_issues"},
		{"/traces/abc123", "performance", "nav.transactions"},
		{"/monitors/7", "uptime", "nav.monitors"},
		{"/projects/5/web-vitals", "performance", "nav.webvitals"},
		{"/projects/5/metrics/alerts", "alerts", "nav.metric_alerts"},
		{"/orgs/7/settings", "settings", "nav.members"},
		{"/orgs/7/teams", "settings", "nav.teams"},
		{"/orgs/7/probes", "settings", "nav.probes"},
	}
	for _, c := range cases {
		// CanManage/CanOperate: без них эти пункты отфильтрованы, и подсвечивать
		// было бы нечего.
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
