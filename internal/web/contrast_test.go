package web

import (
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

// TestControlsUseControlBorder: правило с большей специфичностью не должно
// возвращать интерактивному элементу декоративную границу.
//
// Так и было: --border-control завели под WCAG 1.4.11, а фильтры проблем,
// вкладки, чипы и переключатели языка/темы продолжали брать --border —
// измерено 1.40:1 в тёмной теме.
func TestControlsUseControlBorder(t *testing.T) {
	css := mustAppCSS(t)
	interactive := []string{
		".lang-switcher button",
		".chip {",
		".tabs {",
	}
	for _, sel := range interactive {
		i := strings.Index(css, sel)
		if i < 0 {
			t.Errorf("селектор %q не найден — тест проверяет несуществующее", sel)
			continue
		}
		block := css[i:]
		if end := strings.Index(block, "}"); end > 0 {
			block = block[:end]
		}
		if strings.Contains(block, "var(--border)") {
			t.Errorf("%s берёт декоративную границу --border вместо --border-control:\n%s",
				sel, strings.TrimSpace(block))
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
