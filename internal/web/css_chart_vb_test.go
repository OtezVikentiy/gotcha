package web

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Сторож рассинхрона «ширина viewBox графика ↔ правила кегля подписей».
//
// svgRoot проставляет каждому графику класс chart-vb<ширинаViewBox>, и кегль
// подписей осей задаётся в app.css ИМЕННО по этому классу: font-size живёт в
// единицах viewBox, а на экране равен font-size × ширинаКарточки/ширинаViewBox,
// поэтому один и тот же кегль у графиков разной ширины выглядит по-разному.
// Ширина графика меняется в Go — а правила остаются в CSS, и без сторожа
// рассинхрон не виден: график с незнакомой шириной молча проваливается на
// фолбэк `.metric-chart text { font-size: var(--fs-xs) }`, что на десктопе
// почти совпадает с целью, а на телефоне даёт нечитаемые ~4px. Ровно так и
// случилось с графиками карточки хоста (960): браузерная приёмка на десктопе
// пропустила.
//
// Тест живёт в пакете web, а не в internal/guards, потому что источник истины —
// сами Go-константы ширин, и здесь они доступны напрямую, без разбора AST.

// chartTextViewBoxWidths — ширины графиков, У КОТОРЫХ ЕСТЬ подписи осей
// (<text>), то есть те, для кого кегль по chart-vb вообще что-то значит.
// Спарклайны, флеймграф, вотерфол и мини-график Web Vital сюда не входят: у
// них нет осей, только <title> для мыши.
var chartTextViewBoxWidths = map[string]int{
	"график ряда на странице метрики":   metricChartWidth,
	"графики карточки хоста":            hostChartWidth,
	"частота событий на странице issue": chartWidth,
	"задержки на странице монитора":     latencyChartWidth,
	"перцентили/throughput эндпойнта":   perfLatencyChartWidth,
}

var (
	cssCommentRe  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	chartVBTextRe = regexp.MustCompile(`^\.chart-vb(\d+)\s+text$`)
)

// cssRule — одно правило и ближайшее объемлющее @media-условие.
type cssRule struct {
	selector string
	media    string // "" — правило вне @media
}

// parseCSSRules разбирает таблицу стилей до пар «селектор → @media-контекст».
// Полноценный парсер CSS здесь не нужен: достаточно следить за вложенностью
// фигурных скобок и помнить ближайшую @media-прелюдию. Комментарии срезаются
// заранее — в них встречаются фигурные скобки (например, плейсхолдеры i18n
// вида {name}), которые иначе сбили бы счёт вложенности.
func parseCSSRules(css string) []cssRule {
	css = cssCommentRe.ReplaceAllString(css, " ")

	var (
		rules []cssRule
		stack []string
		start int
	)
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	for i := 0; i < len(css); i++ {
		switch css[i] {
		case '{':
			prelude := norm(css[start:i])
			if !strings.HasPrefix(prelude, "@") {
				media := ""
				for j := len(stack) - 1; j >= 0; j-- {
					if strings.HasPrefix(stack[j], "@media") {
						media = stack[j]
						break
					}
				}
				rules = append(rules, cssRule{selector: prelude, media: media})
			}
			stack = append(stack, prelude)
			start = i + 1
		case '}':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			start = i + 1
		case ';':
			start = i + 1
		}
	}
	return rules
}

// chartVBTextContexts собирает: ширина viewBox → набор @media-контекстов, в
// которых для неё объявлен кегль подписей.
func chartVBTextContexts(css string) map[int]map[string]bool {
	out := map[int]map[string]bool{}
	for _, r := range parseCSSRules(css) {
		for _, sel := range strings.Split(r.selector, ",") {
			m := chartVBTextRe.FindStringSubmatch(strings.TrimSpace(sel))
			if m == nil {
				continue
			}
			w, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			if out[w] == nil {
				out[w] = map[string]bool{}
			}
			out[w][r.media] = true
		}
	}
	return out
}

func TestChartViewBoxFontSizeRules(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("читаю app.css: %v", err)
	}

	got := chartVBTextContexts(css)
	if len(got) == 0 {
		t.Fatal("в app.css нет ни одного правила .chart-vb<ширина> text — " +
			"кегль подписей осей задаётся именно ими")
	}

	// Эталон брейкпойнтов — объединение по всем объявленным ширинам: набор
	// ступеней задаётся самим CSS, и добавление новой ступени автоматически
	// становится обязательным для каждой ширины, а не забывается для одной.
	want := map[string]bool{}
	for _, ctxs := range got {
		for c := range ctxs {
			want[c] = true
		}
	}
	if !want[""] {
		t.Error("нет базового правила .chart-vb<ширина> text вне @media: " +
			"на самых узких экранах кегль возьмётся из фолбэка --fs-xs")
	}
	if len(want) < 2 {
		t.Errorf("ступеней кегля всего %d — блок chart-vb должен перекрывать "+
			"брейкпойнты, иначе подписи не масштабируются", len(want))
	}

	for name, w := range chartTextViewBoxWidths {
		ctxs := got[w]
		if len(ctxs) == 0 {
			t.Errorf("%s: ширина viewBox %d, но в app.css нет ни одного правила "+
				".chart-vb%d text — подписи осей провалятся на фолбэк "+
				".metric-chart text { font-size: var(--fs-xs) } (~4px на телефоне)",
				name, w, w)
			continue
		}
		for _, missing := range sortedMissing(want, ctxs) {
			where := "вне @media"
			if missing != "" {
				where = missing
			}
			t.Errorf("%s: для .chart-vb%d text нет правила в ступени %q — "+
				"остальные ширины его имеют, кегль на этой ступени уедет",
				name, w, where)
		}
	}
}

func sortedMissing(want, got map[string]bool) []string {
	var missing []string
	for c := range want {
		if !got[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	return missing
}
