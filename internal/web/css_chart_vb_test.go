package web

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

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

type cssRule struct {
	selector string
	media    string // "" — правило вне @media
}

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
