package templates

import (
	"strings"
	"testing"
)

func TestStatusTabsCarryAllExplicitly(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"perf-issues", perfStatusFilterURL(7, "all")},
		{"regressions", regressionStatusFilterURL(7, "all")},
		{"profile-regressions", profileRegStatusURL(7, "all")},
	}
	for _, c := range cases {
		if !strings.Contains(c.url, "status=all") {
			t.Errorf("%s: вкладка «Все» ведёт на %q — без status=all хендлер вернёт дефолтный фильтр", c.name, c.url)
		}
	}
}
