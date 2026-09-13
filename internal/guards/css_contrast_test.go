package guards

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	wcagTextMin      = 4.5
	wcagLargeTextMin = 3.0
	wcagNonTextMin   = 3.0
)

type contrastRole string

const (
	roleText      contrastRole = "text"
	roleLargeText contrastRole = "largeText"
	roleUIBorder  contrastRole = "uiBorder"
)

func (r contrastRole) threshold() float64 {
	switch r {
	case roleLargeText:
		return wcagLargeTextMin
	case roleUIBorder:
		return wcagNonTextMin
	default:
		return wcagTextMin
	}
}

// Подложки, на которых селектор реально может показаться, — это не вывести
// из CSS надёжно; токен текста и выражение фона читаются машинно из app.css.
type contrastPair struct {
	Selector string
	Parents  []string
	Role     contrastRole
}

var canonicalParents = []string{"--surface", "--surface-2", "--bg"}

var contrastPairs = []contrastPair{
	{".error", canonicalParents, roleText},
	{".badge-danger", canonicalParents, roleText},
	{".status-overall-major", canonicalParents, roleText},
	{".logs-filter-chip--not", canonicalParents, roleText},
	{".btn-danger-ghost:hover", canonicalParents, roleText},
	{".notice--warn", canonicalParents, roleText},
	{".warning", canonicalParents, roleText},
	{".flash--warn", canonicalParents, roleText},
	{".badge-warn", canonicalParents, roleText},
	{".badge-good", canonicalParents, roleText},
	{".status-overall-ok", canonicalParents, roleText},
	{".flash", canonicalParents, roleText},
	{".notice, .logs-default-notice", canonicalParents, roleText},
	{".badge-info", canonicalParents, roleText},
	{".status-overall-partial", canonicalParents, roleText},
	{".avatar", canonicalParents, roleText},
}

// Ниже факта (16 строк сегодня) — падение сигналит, что строки удалили молча,
// а не что дисциплина ослабла: список растёт вместе с новыми ink-применениями.
const minContrastPairs = 16

// Ниже факта (6 токенов сегодня: accent/good/warn/partial/danger/info).
const minInkTokens = 6

var inkTokenRe = regexp.MustCompile(`^--[\w-]+-ink$`)

func inkTokenNames(tokens map[string]string) []string {
	var out []string
	for name := range tokens {
		if inkTokenRe.MatchString(name) {
			out = append(out, name)
		}
	}
	return out
}

type rgbColor struct{ r, g, b float64 }

var hexColorRe = regexp.MustCompile(`^#([0-9a-fA-F]{6})$`)
var rgbaColorRe = regexp.MustCompile(`^rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)\s*(?:,\s*([\d.]+)\s*)?\)$`)

// alpha=1 для непрозрачных значений (hex) — вызывающий код не обязан отдельно
// проверять прозрачность там, где токен всегда сплошной.
func parseColor(s string) (c rgbColor, alpha float64, ok bool) {
	s = strings.TrimSpace(s)
	if m := hexColorRe.FindStringSubmatch(s); m != nil {
		v, err := strconv.ParseUint(m[1], 16, 32)
		if err != nil {
			return rgbColor{}, 0, false
		}
		return rgbColor{float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)}, 1, true
	}
	if m := rgbaColorRe.FindStringSubmatch(s); m != nil {
		r, _ := strconv.ParseFloat(m[1], 64)
		g, _ := strconv.ParseFloat(m[2], 64)
		b, _ := strconv.ParseFloat(m[3], 64)
		a := 1.0
		if m[4] != "" {
			a, _ = strconv.ParseFloat(m[4], 64)
		}
		return rgbColor{r, g, b}, a, true
	}
	return rgbColor{}, 0, false
}

// Раскручивает цепочку var(--a) -> var(--b) -> значение (например --warn-ink в
// тёмной теме — это буквально var(--warn)); seen рвёт случайный цикл, а не паникует.
func resolveTokenValue(tokens map[string]string, name string) string {
	val := tokens[name]
	seen := map[string]bool{name: true}
	for {
		val = strings.TrimSpace(val)
		m := varRefRe.FindStringSubmatch(val)
		if m == nil {
			return val
		}
		if seen[m[1]] {
			return ""
		}
		seen[m[1]] = true
		val = tokens[m[1]]
	}
}

var varRefRe = regexp.MustCompile(`^var\((--[\w-]+)\)$`)

func resolveColorToken(tokens map[string]string, name string) (rgbColor, float64, error) {
	val := resolveTokenValue(tokens, name)
	c, a, ok := parseColor(val)
	if !ok {
		return rgbColor{}, 0, fmt.Errorf("токен %s = %q не разобран как цвет", name, val)
	}
	return c, a, nil
}

func blendOver(fg rgbColor, alpha float64, bg rgbColor) rgbColor {
	return rgbColor{
		r: alpha*fg.r + (1-alpha)*bg.r,
		g: alpha*fg.g + (1-alpha)*bg.g,
		b: alpha*fg.b + (1-alpha)*bg.b,
	}
}

var colorMixRe = regexp.MustCompile(`^color-mix\(in srgb,\s*var\((--[\w-]+)\)\s*(\d+)%,\s*transparent\)$`)

// Понимает две формы фона: color-mix(..., transparent) поверх подложки и
// одиночный var(--token), который сам может резолвиться в rgba() со своей альфой.
func resolveBackground(tokens map[string]string, expr string, parent rgbColor) (rgbColor, error) {
	expr = strings.TrimSpace(expr)
	if m := colorMixRe.FindStringSubmatch(expr); m != nil {
		tint, tintAlpha, err := resolveColorToken(tokens, m[1])
		if err != nil {
			return rgbColor{}, err
		}
		pct, err := strconv.Atoi(m[2])
		if err != nil {
			return rgbColor{}, err
		}
		return blendOver(tint, tintAlpha*float64(pct)/100, parent), nil
	}
	if m := varRefRe.FindStringSubmatch(expr); m != nil {
		c, a, err := resolveColorToken(tokens, m[1])
		if err != nil {
			return rgbColor{}, err
		}
		if a >= 1 {
			return c, nil
		}
		return blendOver(c, a, parent), nil
	}
	return rgbColor{}, fmt.Errorf("не умею разбирать выражение фона %q", expr)
}

func srgbChannel(c float64) float64 {
	c /= 255
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func relativeLuminance(c rgbColor) float64 {
	return 0.2126*srgbChannel(c.r) + 0.7152*srgbChannel(c.g) + 0.0722*srgbChannel(c.b)
}

// WCAG 2.x, формула 1.4.3: (L1+0.05)/(L2+0.05), L1 — светлее.
func contrastRatio(a, b rgbColor) float64 {
	la, lb := relativeLuminance(a)+0.05, relativeLuminance(b)+0.05
	if la < lb {
		la, lb = lb, la
	}
	return la / lb
}

func rootTokenBlocks(t *testing.T, tree *Tree) (dark, light map[string]string) {
	t.Helper()
	blocks := parseCSSBlocks(tree.CSS.Body)
	for _, b := range blocks {
		if b.AtRule != "" {
			continue
		}
		switch b.Selector {
		case ":root":
			dark = parseCSSDeclarations(b.Body)
		case `:root[data-theme="light"]`:
			light = parseCSSDeclarations(b.Body)
		}
	}
	if dark == nil || light == nil {
		t.Fatalf("не нашли блоки :root / :root[data-theme=\"light\"] — разбор app.css сломан")
	}
	return dark, light
}

// Последнее объявление свойства побеждает в каскаде — как в parseCSSDeclarations,
// но с explicit ok: у блока может не быть ни color, ни background вовсе.
func declValue(body, prop string) (string, bool) {
	v, ok := parseCSSDeclarations(body)[prop]
	return v, ok
}

func selectorBlock(t *testing.T, blocks []cssBlock, selector string) cssBlock {
	t.Helper()
	for _, b := range blocks {
		if b.AtRule == "" && b.Selector == selector {
			return b
		}
	}
	t.Fatalf("селектор %q не найден в app.css вне @media — правило переименовали или удалили, contrastPairs надо обновить", selector)
	return cssBlock{}
}

func TestContrastPairsMeetWCAG(t *testing.T) {
	tree := Load(t)
	dark, light := rootTokenBlocks(t, tree)
	blocks := parseCSSBlocks(tree.CSS.Body)

	if len(contrastPairs) < minContrastPairs {
		t.Fatalf("в contrastPairs %d строк, ожидалось не меньше %d — список ужали, а не сократили состав стилей", len(contrastPairs), minContrastPairs)
	}

	inkTokens := inkTokenNames(dark)
	if len(inkTokens) < minInkTokens {
		t.Fatalf("в :root нашлось %d токенов *-ink, ожидалось не меньше %d — разбор app.css сломан", len(inkTokens), minInkTokens)
	}

	referenced := map[string]bool{}
	themes := []struct {
		name   string
		tokens map[string]string
	}{
		{"тёмная", dark},
		{"светлая", light},
	}
	for _, p := range contrastPairs {
		block := selectorBlock(t, blocks, p.Selector)
		colorExpr, ok := declValue(block.Body, "color")
		if !ok {
			t.Errorf("%s: в блоке нет объявления color — нечего проверять на контраст", p.Selector)
			continue
		}
		m := varRefRe.FindStringSubmatch(strings.TrimSpace(colorExpr))
		if m == nil {
			t.Errorf("%s: color: %s — ожидался одиночный var(--token), контраст не проверить", p.Selector, colorExpr)
			continue
		}
		referenced[m[1]] = true

		bgExpr, ok := declValue(block.Body, "background")
		if !ok {
			bgExpr, ok = declValue(block.Body, "background-color")
		}
		if !ok {
			t.Errorf("%s: в блоке нет объявления фона — контраст text-на-подложке не определён", p.Selector)
			continue
		}

		for _, theme := range themes {
			textColor, _, err := resolveColorToken(theme.tokens, m[1])
			if err != nil {
				t.Errorf("[%s] %s: %v", theme.name, p.Selector, err)
				continue
			}
			for _, parentName := range p.Parents {
				parentColor, _, err := resolveColorToken(theme.tokens, parentName)
				if err != nil {
					t.Errorf("[%s] %s: подложка %s: %v", theme.name, p.Selector, parentName, err)
					continue
				}
				bgColor, err := resolveBackground(theme.tokens, bgExpr, parentColor)
				if err != nil {
					t.Errorf("[%s] %s: %v", theme.name, p.Selector, err)
					continue
				}
				ratio := contrastRatio(textColor, bgColor)
				if threshold := p.Role.threshold(); ratio < threshold {
					t.Errorf("[%s] %s: color:%s на фоне %q поверх %s даёт %.3f:1, порог %.1f:1",
						theme.name, p.Selector, m[1], bgExpr, parentName, ratio, threshold)
				}
			}
		}
	}

	for _, ink := range inkTokens {
		if !referenced[ink] {
			t.Errorf("токен %s не встречается как color ни в одном селекторе из contrastPairs — контраст его применений не проверяется", ink)
		}
	}
}
