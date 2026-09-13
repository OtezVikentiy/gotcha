package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var chartVBFontRuleRe = regexp.MustCompile(`\.chart-vb(\d+) text \{ font-size: (\d+)px; \}`)

// правила ВНЕ @media — базовая (безмедийная, <700px) ступень; на ней кегль
// относительно viewBox наибольший из всех тиров, оттуда и калибровка.
func stripMediaBlocks(css string) string {
	return regexp.MustCompile(`(?s)@media[^{]*\{.*?\n\}`).ReplaceAllString(css, " ")
}

func TestSvgCharWidthMatchesCSSTier(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("читаю app.css: %v", err)
	}
	css = cssCommentRe.ReplaceAllString(css, " ")

	base := stripMediaBlocks(css)
	calibrated := map[int]bool{}
	for _, m := range chartVBFontRuleRe.FindAllStringSubmatch(base, -1) {
		w, _ := strconv.Atoi(m[1])
		font, _ := strconv.Atoi(m[2])
		want := 0.6024 * float64(font)
		got := svgCharWidthPx(w)
		if math.Abs(got-want)/want > 0.02 {
			t.Errorf(".chart-vb%d text на базовой ступени (<700px): кегль %dpx → ширина руны %.2f, "+
				"а svgCharWidthPx(%d) = %.2f (расхождение > 2%%) — калибровка разошлась с CSS",
				w, font, want, w, got)
		}
		calibrated[w] = true
	}
	if len(calibrated) == 0 {
		t.Fatal("на базовой ступени (<700px) нет ни одного правила .chart-vbN text")
	}
	for name, w := range chartTextViewBoxWidths {
		if !calibrated[w] {
			t.Errorf("%s: ширина viewBox %d без правила .chart-vb%d text на базовой ступени — "+
				"калибровка svgCharWidthPerVB для неё не сверена", name, w, w)
		}
	}

	// ни один более узкий кегль (widescreen-тиры) не должен требовать больше
	// оценки — иначе подпись обрежется на этой ступени.
	for _, m := range chartVBFontRuleRe.FindAllStringSubmatch(css, -1) {
		w, _ := strconv.Atoi(m[1])
		font, _ := strconv.Atoi(m[2])
		want := 0.6024 * float64(font)
		got := svgCharWidthPx(w)
		if got+0.01 < want {
			t.Errorf(".chart-vb%d text: кегль %dpx требует %.2f на руну, а оценка svgCharWidthPx(%d) = %.2f — "+
				"на этом тире подпись обрежется", w, font, want, w, got)
		}
	}
}

// вотерфол (viewBox 900) намеренно вне тиров chart-vbN: такое правило
// специфичнее .waterfall-label и молча заменило бы его кегль тировым.
func TestWaterfallOptsOutOfChartVBTier(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("читаю app.css: %v", err)
	}
	css = cssCommentRe.ReplaceAllString(css, " ")

	forbidden := ".chart-vb" + strconv.Itoa(waterfallWidth) + " text"
	if strings.Contains(css, forbidden) {
		t.Errorf("в app.css появилось правило %q — оно перебьёт .waterfall-label по специфичности "+
			"(0,1,1 против 0,1,0) и заменит фиксированный кегль тировым", forbidden)
	}
	if !strings.Contains(css, ".waterfall-label") {
		t.Fatal("в app.css нет правила .waterfall-label — кегль подписей вотерфола ничем не задан")
	}
}
