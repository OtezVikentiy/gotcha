package guards

import (
	"regexp"
	"strings"
	"testing"
)

// Скролл-контейнер, свёрстанный литеральным <div class="table-scroll">,
// минует scrollRegion и теряет tabindex/role/aria-label — клавиатура не может
// прокрутить содержимое (WCAG 2.1.1, №31). Единственная точка входа —
// компонент scrollRegion (scroll.templ).
var scrollClassRe = regexp.MustCompile(`class="[^"]*\b(table-scroll|scroll-list|endpoint-chart|metric-chart-wrap|flamegraph-wrap|trace-waterfall|trace-flame|issue-chart)\b[^"]*"`)

// minScrollRegionUses — нижняя граница числа употреблений компонента:
// фактически 40 на момент введения; порог с запасом вниз (приём
// minBlocksInsideMedia) ловит слепоту обхода, а не колебания вёрстки.
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
