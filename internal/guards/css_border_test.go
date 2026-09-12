package guards

import (
	"regexp"
	"strings"
	"testing"
)

// AtRule хранит текст ближайшего оборачивающего @-правила — отличает
// одинаковый селектор в разных медиа-контекстах.
type cssBlock struct {
	Selector string
	Body     string
	Line     int
	AtRule   string
}

// Ключ в seen/Exemption.Value иначе был бы хрупкой строкой с табами и переносами.
var cssWhitespaceRe = regexp.MustCompile(`\s+`)

func normalizeSelector(sel string) string {
	return strings.TrimSpace(cssWhitespaceRe.ReplaceAllString(sel, " "))
}

// Заменяет комментарий на пробелы посимвольно, сохраняя '\n' — иначе номера
// строк после многострочного комментария съезжали бы вверх.
func stripCSSCommentsKeepLines(css string) string {
	return cssCommentRe.ReplaceAllStringFunc(css, func(m string) string {
		var b strings.Builder
		b.Grow(len(m))
		for _, r := range m {
			if r == '\n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		}
		return b.String()
	})
}

// @media/@keyframes разворачиваются рекурсивно — их "селектор" в список не
// попадает, вложенные объявления попадают как обычные блоки.
func parseCSSBlocks(css string) []cssBlock {
	var out []cssBlock
	parseCSSBlocksInto(stripCSSCommentsKeepLines(css), 1, "", &out)
	return out
}

func parseCSSBlocksInto(css string, startLine int, atRule string, out *[]cssBlock) {
	pos := 0
	line := startLine
	for pos < len(css) {
		rest := css[pos:]
		openIdx := strings.IndexByte(rest, '{')
		if openIdx < 0 {
			return
		}
		selector := strings.TrimSpace(rest[:openIdx])
		selectorLine := line + strings.Count(rest[:openIdx], "\n")

		// Считаем вложенность { } — только у @media/@keyframes внутри тела свои.
		depth := 1
		end := openIdx + 1
		for depth > 0 {
			tail := rest[end:]
			nextOpen := strings.IndexByte(tail, '{')
			nextClose := strings.IndexByte(tail, '}')
			if nextClose < 0 {
				// Несбалансированный CSS — молча останавливаемся, не паникуем.
				return
			}
			if nextOpen >= 0 && nextOpen < nextClose {
				depth++
				end += nextOpen + 1
			} else {
				depth--
				end += nextClose + 1
			}
		}
		body := rest[openIdx+1 : end-1]

		if strings.HasPrefix(selector, "@") {
			parseCSSBlocksInto(body, selectorLine, normalizeSelector(selector), out)
		} else if selector != "" {
			*out = append(*out, cssBlock{Selector: normalizeSelector(selector), Body: body, Line: selectorLine, AtRule: atRule})
		}

		line += strings.Count(rest[:end], "\n")
		pos += end
	}
}

// Граница здесь функциональна, а не декоративна (WCAG 1.4.11, порог 3:1).
// Список — часть правила, а не исключений: добавление сюда ужесточает проверку.
var interactiveSelectors = []string{
	"input", "select", "textarea", "button", "summary",
	".chip", ".tab", "[class*=tab]", ".segmented", ".dr-",
	".lang-switcher", ".theme-toggle",
}

// Ведущую точку у ".tab"/".chip" отбрасываем и ищем по границе слова (\b) —
// иначе ".tab" не найдёт .kind-tab, а голая подстрока поймает .data-table.
var interactiveMarkerRes = func() []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(interactiveSelectors))
	for i, m := range interactiveSelectors {
		stripped := strings.TrimPrefix(m, ".")
		res[i] = regexp.MustCompile(`\b` + regexp.QuoteMeta(stripped) + `\b`)
	}
	return res
}()

// Более широкое совпадение здесь безопасно: блок без var(--border) просто
// не станет нарушением на следующей проверке.
func selectorIsInteractive(selector string) bool {
	for _, re := range interactiveMarkerRes {
		if re.MatchString(selector) {
			return true
		}
	}
	return false
}

// border-radius/border-collapse/border-spacing не входят — не цвет и не контраст.
var borderColorProps = map[string]bool{
	"border":              true,
	"border-color":        true,
	"border-top":          true,
	"border-right":        true,
	"border-bottom":       true,
	"border-left":         true,
	"border-top-color":    true,
	"border-right-color":  true,
	"border-bottom-color": true,
	"border-left-color":   true,
}

// Берёт ПОСЛЕДНЕЕ по тексту объявление внутри блока — оно побеждает в
// каскаде, если border и border-color заданы дважды.
func lastBorderColorValue(body string) (value string, ok bool) {
	for _, stmt := range strings.Split(body, ";") {
		i := strings.IndexByte(stmt, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(stmt[:i])
		if borderColorProps[name] {
			value = strings.TrimSpace(stmt[i+1:])
			ok = true
		}
	}
	return value, ok
}

// Список пуст и должен оставаться пустым — новая запись требует явного
// решения с обоснованием.
var debtControlBorderExemptions = []Exemption{}

const maxDebtControlBorderExemptions = 0

// @keyframes не считаются — у кадров анимации нет border, только @media.
func blocksInsideMedia(blocks []cssBlock) int {
	n := 0
	for _, b := range blocks {
		if strings.HasPrefix(b.AtRule, "@media") {
			n++
		}
	}
	return n
}

// Порог ниже факта (33 блока в 13 разных @media) — ловит обвал разбора
// внутри @media, а не обычные колебания состава стилей при правках вёрстки.
const minBlocksInsideMedia = 20

func TestControlsUseControlBorder(t *testing.T) {
	tree := Load(t)
	blocks := parseCSSBlocks(tree.CSS.Body)

	// Долговой список ниже это не поймает — все его записи есть и вне @media.
	if n := blocksInsideMedia(blocks); n < minBlocksInsideMedia {
		t.Fatalf("разбор нашёл %d блоков внутри @media, ожидалось не меньше %d — это регрессия разбора @media, а не изменение состава стилей", n, minBlocksInsideMedia)
	}

	exempt := ExemptedValues(debtControlBorderExemptions)
	seen := map[string]bool{}
	reported := map[string]bool{}

	for _, b := range blocks {
		if !selectorIsInteractive(b.Selector) {
			continue
		}
		value, ok := lastBorderColorValue(b.Body)
		if !ok || !strings.Contains(value, "var(--border)") {
			continue
		}
		seen[b.Selector] = true
		if exempt[b.Selector] || reported[b.Selector] {
			continue
		}
		reported[b.Selector] = true
		t.Errorf("app.css:%d: %s берёт декоративную границу --border вместо --border-control (WCAG 1.4.11): %s",
			b.Line, b.Selector, value)
	}

	CheckExemptions(t, "TestControlsUseControlBorder (долг подпроекта G)", debtControlBorderExemptions, maxDebtControlBorderExemptions, seen)
}
