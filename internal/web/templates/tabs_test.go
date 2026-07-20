package templates

import (
	"strings"
	"testing"
)

// TestStatusTabsCarryAllExplicitly — вкладка «Все» должна нести status=all в
// адресе. Раньше общий хелпер tabQuery заменял «all» на пустую строку, считая
// «без фильтра» значением по умолчанию, а хендлеры считают своим дефолтом
// «открытые»/«нерешённые». Из-за расхождения клик по «Все» возвращал на
// первую вкладку — во всех трёх разделах сразу.
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
