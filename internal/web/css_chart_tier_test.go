package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Сторож калибровки ширины руны подписей осей (svgCharWidthPerVB) против
// CSS-ступени, с которой она снята: @media(min-width:700px) в app.css.
//
// Генератор SVG считает ширину подписей (прижим оси Y, разводка тиков X,
// антиколлизия подписей деплоя) по одной константе на единицу ширины
// viewBox; правда о кегле живёт в app.css (.chart-vb<N> text). Разойдутся —
// подписи снова начнут резаться или слипаться, и ни один Go-тест этого не
// заметит: числа-то внутри пакета согласованы. Замер ширины руны — 0.6024
// кегля на --font-mono (см. докблок svgCharWidthPerVB).
func TestSvgCharWidthMatchesCSSTier(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("читаю app.css: %v", err)
	}
	css = cssCommentRe.ReplaceAllString(css, " ")

	// Блоков @media (min-width: 700px) в app.css несколько — правила кегля
	// собираются по всем, а не по первому попавшемуся.
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
	// Каждый график с подписями осей (список из css_chart_vb_test.go) обязан
	// попадать в откалиброванный тир, а не только те три, что есть сегодня.
	for name, w := range chartTextViewBoxWidths {
		if !calibrated[w] {
			t.Errorf("%s: ширина viewBox %d без правила .chart-vb%d text на ступени ≥700px — "+
				"калибровка svgCharWidthPerVB для неё не сверена", name, w, w)
		}
	}
}
