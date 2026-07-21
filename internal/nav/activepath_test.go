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
		{"/issues/42", "issues", "nav.issues"},
		{"/perf-issues/218", "performance", "nav.perf_issues"},
		{"/traces/abc123", "performance", "nav.transactions"},
		{"/monitors/7", "uptime", "nav.monitors"},
		// Обычные пути продолжают работать как раньше.
		{"/projects/5/web-vitals", "performance", "nav.webvitals"},
		{"/projects/5/metrics/alerts", "metrics", "nav.metric_alerts"},
	}
	for _, c := range cases {
		items := Subsections(Shell{ProjectID: 5, Area: c.area, Path: c.path})
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
