package web

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Токены цвета правились по измерениям, а не на глаз, и без стража эти
// измерения живут только в комментариях: следующая правка палитры откатит их
// молча. Тест считает контраст по формуле WCAG прямо из app.css.

// tokenRe вытаскивает значение токена из блока темы.
var tokenRe = regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})\s*;`)

// themeTokens разбирает токены темы из app.css: dark — из :root, light — из
// :root[data-theme="light"].
func themeTokens(t *testing.T, theme string) map[string]string {
	t.Helper()
	css := mustAppCSS(t)
	var block string
	switch theme {
	case "dark":
		start := strings.Index(css, ":root {")
		if start < 0 {
			t.Fatal("не найден блок :root")
		}
		end := strings.Index(css[start:], "\n}")
		block = css[start : start+end]
	case "light":
		start := strings.Index(css, `:root[data-theme="light"] {`)
		if start < 0 {
			t.Fatal(`не найден блок :root[data-theme="light"]`)
		}
		end := strings.Index(css[start:], "\n}")
		block = css[start : start+end]
	}
	out := map[string]string{}
	for _, m := range tokenRe.FindAllStringSubmatch(block, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatalf("в блоке темы %s не разобрано ни одного токена", theme)
	}
	return out
}

// mustAppCSS — таблица стилей для тестов контраста; общий readAppCSS отдаёт
// ошибку, здесь она означает сломанный запуск теста.
func mustAppCSS(t *testing.T) string {
	t.Helper()
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	return css
}

func channel(v float64) float64 {
	v /= 255
	if v <= 0.03928 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func luminance(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	r, _ := strconv.ParseInt(hex[0:2], 16, 32)
	g, _ := strconv.ParseInt(hex[2:4], 16, 32)
	b, _ := strconv.ParseInt(hex[4:6], 16, 32)
	return 0.2126*channel(float64(r)) + 0.7152*channel(float64(g)) + 0.0722*channel(float64(b))
}

func contrast(fg, bg string) float64 {
	l1, l2 := luminance(fg), luminance(bg)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// TestTextTokensMeetContrast: цвета текста обязаны давать 4.5:1 на подложках,
// на которых их реально рисуют (WCAG 1.4.3).
//
// --text-mute проверяется именно здесь: «Не назначено» и заголовки дней недели
// в пикере давали 2.7-3.1:1 в тёмной теме, то есть подпись была почти не видна.
func TestTextTokensMeetContrast(t *testing.T) {
	const want = 4.5
	surfaces := []string{"bg", "surface", "surface-2"}
	for _, theme := range []string{"dark", "light"} {
		tokens := themeTokens(t, theme)
		for _, fg := range []string{"text", "text-dim", "text-mute", "link"} {
			for _, bgName := range surfaces {
				fgHex, okFG := tokens[fg]
				bgHex, okBG := tokens[bgName]
				if !okFG || !okBG {
					t.Fatalf("[%s] нет токена %s или %s", theme, fg, bgName)
				}
				if got := contrast(fgHex, bgHex); got < want {
					t.Errorf("[%s] --%s на --%s = %.2f:1, нужно %.1f:1",
						theme, fg, bgName, got, want)
				}
			}
		}
	}
}

// TestControlBorderMeetsContrast: граница интерактивного элемента несёт смысл
// («вот докуда контрол»), поэтому обязана давать 3:1 (WCAG 1.4.11).
func TestControlBorderMeetsContrast(t *testing.T) {
	const want = 3.0
	for _, theme := range []string{"dark", "light"} {
		tokens := themeTokens(t, theme)
		for _, bgName := range []string{"bg", "surface", "surface-2"} {
			got := contrast(tokens["border-control"], tokens[bgName])
			if got < want {
				t.Errorf("[%s] --border-control на --%s = %.2f:1, нужно %.1f:1",
					theme, bgName, got, want)
			}
		}
	}
}

// TestAvailabilityBarFillTokens: заливки полоски доступности — не текст, их
// светлоту можно и нужно разводить (№28): попарно ≥3:1, чтобы состояния
// различались и без цветового зрения, и ≥1.5:1 к карточке, чтобы корзина не
// сливалась с фоном. Глобальные --good/--partial/--danger прижаты к текстовому
// 4.5:1 и разводке не подлежат — поэтому у полоски свои токены.
func TestAvailabilityBarFillTokens(t *testing.T) {
	for _, theme := range []string{"dark", "light"} {
		tokens := themeTokens(t, theme)
		up, pa, dn := tokens["bar-up"], tokens["bar-partial"], tokens["bar-down"]
		if up == "" || pa == "" || dn == "" {
			t.Fatalf("[%s] нет токенов --bar-up/--bar-partial/--bar-down", theme)
		}
		pairs := []struct{ a, b, name string }{
			{up, pa, "up~partial"}, {pa, dn, "partial~down"}, {up, dn, "up~down"},
		}
		for _, p := range pairs {
			if got := contrast(p.a, p.b); got < 3.0 {
				t.Errorf("[%s] %s = %.2f:1, нужно ≥3:1", theme, p.name, got)
			}
		}
		for name, hex := range map[string]string{"bar-up": up, "bar-partial": pa, "bar-down": dn} {
			if got := contrast(hex, tokens["surface"]); got < 1.5 {
				t.Errorf("[%s] --%s на --surface = %.2f:1, нужно ≥1.5:1", theme, name, got)
			}
		}
	}
}

// rgbaTokenRe — полупрозрачные токены вида rgba(96, 132, 255, .12).
var rgbaTokenRe = regexp.MustCompile(`--([a-z0-9-]+):\s*rgba\((\d+),\s*(\d+),\s*(\d+),\s*([0-9.]+)\)\s*;`)

// compositeToken накладывает полупрозрачный токен на подложку и возвращает
// итоговый непрозрачный hex — то, что реально видит глаз.
func compositeToken(t *testing.T, theme, name, bgHex string) string {
	t.Helper()
	css := mustAppCSS(t)
	var block string
	switch theme {
	case "dark":
		start := strings.Index(css, ":root {")
		block = css[start : start+strings.Index(css[start:], "\n}")]
	case "light":
		start := strings.Index(css, `:root[data-theme="light"] {`)
		block = css[start : start+strings.Index(css[start:], "\n}")]
	}
	for _, m := range rgbaTokenRe.FindAllStringSubmatch(block, -1) {
		if m[1] != name {
			continue
		}
		r, _ := strconv.Atoi(m[2])
		g, _ := strconv.Atoi(m[3])
		b, _ := strconv.Atoi(m[4])
		a, _ := strconv.ParseFloat(m[5], 64)
		br, _ := strconv.ParseInt(strings.TrimPrefix(bgHex, "#")[0:2], 16, 32)
		bg, _ := strconv.ParseInt(strings.TrimPrefix(bgHex, "#")[2:4], 16, 32)
		bb, _ := strconv.ParseInt(strings.TrimPrefix(bgHex, "#")[4:6], 16, 32)
		mix := func(c int, b int64) int64 { return int64(float64(c)*a + float64(b)*(1-a) + 0.5) }
		return fmt.Sprintf("#%02x%02x%02x", mix(r, br), mix(g, bg), mix(b, bb))
	}
	t.Fatalf("[%s] rgba-токен --%s не найден", theme, name)
	return ""
}

// cssRuleBody — тело первого блока по литеральному селектору (для точечных
// проверок «этот селектор больше не делает X»).
func cssRuleBody(css, sel string) string {
	i := strings.Index(css, sel)
	if i < 0 {
		return ""
	}
	j := strings.Index(css[i:], "}")
	return css[i : i+j]
}

// TestSegmentedActiveTextContrast: подпись активного сегмента — текст на
// подложке --accent-soft, ей нужен 4.5:1 (№30). --accent на этой подложке
// давал 2.98:1 в тёмной теме; --link рассчитан как текст и проходит.
func TestSegmentedActiveTextContrast(t *testing.T) {
	css := mustAppCSS(t)
	if !strings.Contains(css, ".segmented label:has(input:checked)") ||
		strings.Contains(cssRuleBody(css, ".segmented label:has(input:checked)"), "var(--accent);") {
		t.Errorf(".segmented активный сегмент всё ещё красит текст в --accent")
	}
	for _, theme := range []string{"dark", "light"} {
		tokens := themeTokens(t, theme)
		soft := compositeToken(t, theme, "accent-soft", tokens["surface"])
		if got := contrast(tokens["link"], soft); got < 4.5 {
			t.Errorf("[%s] --link на --accent-soft⊕--surface = %.2f:1, нужно ≥4.5:1", theme, got)
		}
	}
}

// TestLatencyPhaseTokens: фазы запроса (DNS→TCP→TLS→TTFB) — заливки на
// карточке: каждая ≥3:1 к --surface, соседние различимы (≥1.3:1), светлота
// растёт монотонно — «бренд-градиент кодирует последовательность фаз»
// остаётся правдой в обеих темах (№74: TTFB #c3b8fc давал 1.81:1 на светлой).
func TestLatencyPhaseTokens(t *testing.T) {
	order := []string{"phase-dns", "phase-tcp", "phase-tls", "phase-ttfb"}
	for _, theme := range []string{"dark", "light"} {
		tokens := themeTokens(t, theme)
		for _, name := range order {
			if tokens[name] == "" {
				t.Fatalf("[%s] нет токена --%s", theme, name)
			}
			if got := contrast(tokens[name], tokens["surface"]); got < 3.0 {
				t.Errorf("[%s] --%s на --surface = %.2f:1, нужно ≥3:1", theme, name, got)
			}
		}
		for i := 0; i+1 < len(order); i++ {
			a, b := tokens[order[i]], tokens[order[i+1]]
			if got := contrast(a, b); got < 1.3 {
				t.Errorf("[%s] соседние %s~%s = %.2f:1, нужно ≥1.3:1", theme, order[i], order[i+1], got)
			}
			if luminance(b) <= luminance(a) {
				t.Errorf("[%s] светлота не монотонна: %s ≤ %s", theme, order[i+1], order[i])
			}
		}
	}
	// Оба места отрисовки читают токены, а не литералы — рассинхрон графика
	// с легендой (№74) невозможен по построению.
	css := mustAppCSS(t)
	for _, want := range []string{
		".latency-chart .seg-dns", ".legend-dns::before",
	} {
		if !strings.Contains(cssRuleBody(css, want), "var(--phase-dns)") {
			t.Errorf("%s не использует var(--phase-dns)", want)
		}
	}
}

// TestFlashLeavesAccessibilityTree: автоскрытая плашка обязана уходить из
// дерева доступности, а не только с глаз: opacity:0 оставлял кнопку закрытия в
// порядке табуляции.
func TestFlashLeavesAccessibilityTree(t *testing.T) {
	css := mustAppCSS(t)
	i := strings.Index(css, "@keyframes flash-dismiss")
	if i < 0 {
		t.Fatal("не найдена анимация flash-dismiss")
	}
	block := css[i : i+400]
	if !strings.Contains(block, "visibility: hidden") {
		t.Error("flash-dismiss не выставляет visibility:hidden — скрытая плашка остаётся в табуляции")
	}
	if !strings.Contains(css, "animation-play-state: paused") {
		t.Error("таймер плашки нельзя приостановить (WCAG 2.2.1)")
	}
}
