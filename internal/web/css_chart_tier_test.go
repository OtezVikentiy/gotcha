package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestSvgCharWidthMatchesCSSTier(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("читаю app.css: %v", err)
	}
	css = cssCommentRe.ReplaceAllString(css, " ")

	blocks := regexp.MustCompile(`(?s)@media \(min-width: 700px\) \{(.*?)\n\}`).FindAllStringSubmatch(css, -1)
	if len(blocks) == 0 {
		t.Fatal("в app.css нет блока @media (min-width: 700px) — опорная ступень калибровки")
	}
	var tier strings.Builder
	for _, b := range blocks {
		tier.WriteString(b[1])
	}
	rule := regexp.MustCompile(`\.chart-vb(\d+) text \{ font-size: (\d+)px; \}`)
	calibrated := map[int]bool{}
	for _, m := range rule.FindAllStringSubmatch(tier.String(), -1) {
		w, _ := strconv.Atoi(m[1])
		font, _ := strconv.Atoi(m[2])
		want := 0.6024 * float64(font)
		got := svgCharWidthPx(w)
		if math.Abs(got-want)/want > 0.01 {
			t.Errorf(".chart-vb%d text на ступени ≥700px: кегль %dpx → ширина руны %.2f, "+
				"а svgCharWidthPx(%d) = %.2f (расхождение > 1%%) — калибровка разошлась с CSS",
				w, font, want, w, got)
		}
		calibrated[w] = true
	}
	if len(calibrated) == 0 {
		t.Fatal("на ступени ≥700px нет ни одного правила .chart-vbN text")
	}
	for name, w := range chartTextViewBoxWidths {
		if !calibrated[w] {
			t.Errorf("%s: ширина viewBox %d без правила .chart-vb%d text на ступени ≥700px — "+
				"калибровка svgCharWidthPerVB для неё не сверена", name, w, w)
		}
	}
}
