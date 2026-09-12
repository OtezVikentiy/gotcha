package guards

import (
	"regexp"
	"strings"
	"testing"
)

// литеральный скролл-класс мимо @scrollRegion теряет tabindex/role/aria-label —
// клавиатура не прокрутит содержимое.
var scrollClassRe = regexp.MustCompile(`class="[^"]*\b(table-scroll|scroll-list|endpoint-chart|metric-chart-wrap|flamegraph-wrap|trace-waterfall|trace-flame|issue-chart)\b[^"]*"`)

const minScrollRegionUses = 25

func TestScrollContainersUseScrollRegion(t *testing.T) {
	tree := Load(t)
	uses := 0
	for _, f := range tree.Templates {
		uses += strings.Count(f.Body, "@scrollRegion(")
		if strings.HasSuffix(f.Path, "scroll.templ") {
			continue
		}
		for _, m := range scrollClassRe.FindAllString(f.Body, -1) {
			t.Errorf("%s: литеральный скролл-класс %s — оборачивать в @scrollRegion", f.Path, m)
		}
	}
	if uses < minScrollRegionUses {
		t.Errorf("@scrollRegion употреблён %d раз при пороге ≥%d — обход сломан или конвертацию откатили", uses, minScrollRegionUses)
	}
}
