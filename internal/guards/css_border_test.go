package guards

import (
	"regexp"
	"strings"
	"testing"
)

// cssBlock — один разбор «селектор → тело объявлений» вместе с номером
// строки, на которой начинается селектор в исходном app.css (для сообщений
// об ошибках — человек должен суметь найти блок глазами).
//
// AtRule — нормализованный текст БЛИЖАЙШЕГО оборачивающего @-правила
// ("@media (prefers-color-scheme: light)" и т.п.) или "" для блока верхнего
// уровня. Полю нашлось применение только во втором потребителе разбора
// (TestLightThemeBlocksAgree в css_theme_test.go, задача 7): ему нужно
// отличать блок ВНУТРИ @media (prefers-color-scheme: light) от одноимённого
// селектора в другом медиа-контексте (например, внутри @media (min-width:
// ...)) — то, что плоский список сам по себе после разворачивания @media
// теряет. TestControlsUseControlBorder это поле не читает: ему конкретный
// @media, обернувший блок, не важен.
type cssBlock struct {
	Selector string
	Body     string
	Line     int
	AtRule   string
}

// cssWhitespaceRe схлопывает пробелы/табы/переносы строк в один пробел —
// применяется к селектору ПОСЛЕ разбора, чтобы ключ в seen/Exemption.Value
// был устойчивой однострочной строкой, а не куском исходного текста с
// табами и переносами (у групповых селекторов вида
// ".lang-switcher,\n\t.theme-toggle" сырой текст неудобно и хрупко хранить
// литералом в списке исключений).
var cssWhitespaceRe = regexp.MustCompile(`\s+`)

func normalizeSelector(sel string) string {
	return strings.TrimSpace(cssWhitespaceRe.ReplaceAllString(sel, " "))
}

// stripCSSCommentsKeepLines вычищает /* ... */ так же, как cssCommentRe в
// css_classes_test.go (используем ту же регулярку — переиспользуем разбор
// комментариев, а не заводим второй), но заменяет содержимое комментария на
// пробелы ПОСИМВОЛЬНО, сохраняя переносы строк внутри него, вместо того
// чтобы вырезать текст целиком.
//
// Разница важна: css_classes_test.go номера строк на стороне CSS никогда не
// печатает (там репортится только сторона разметки — .templ/.go, у неё свои
// номера строк, не зависящие от app.css). Этому правилу номер строки в
// app.css нужен в каждом сообщении об ошибке, а обычная вырезка комментария
// (cssCommentRe.ReplaceAllString(css, "")) удаляет вместе с текстом и
// переносы строк ВНУТРИ многострочных комментариев — все номера строк после
// такого комментария съехали бы вверх на его высоту. Посимвольная замена на
// пробел с сохранением '\n' даёт байт-в-байт тот же построчный разбор, что и
// у исходного файла, при том что комментарий по-прежнему не участвует в
// разборе селекторов/объявлений (пробелы не создают ни селекторов, ни
// объявлений).
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

// parseCSSBlocks разбирает app.css на плоский список блоков «селектор →
// объявления». Второй потребитель разбора CSS в пакете (первый —
// cssDefinedClasses в css_classes_test.go, оттуда переиспользована только
// вычистка комментариев, см. stripCSSCommentsKeepLines выше) — держим разбор
// блоков здесь, рядом с единственным правилом, которому он нужен, а не в
// tree.go: Tree — сырое дерево файлов, а разбор конкретного формата (CSS) не
// его забота.
//
// @media и @keyframes разворачиваются рекурсивно: у самого at-правила нет
// объявлений цвета границы, только вложенные правила — сам его "селектор"
// (например "@media (min-width: 900px)") в плоский список не попадает, зато
// всё вложенное содержимое попадает туда же, где лежали бы обычные
// верхнеуровневые блоки. В этом файле оба at-правила с фигурными скобками —
// единственные (`grep -n "^@"` подтверждает: ни @import, ни @font-face, ни
// @supports с точкой-с-запятой вместо блока нет) — разбору не нужно уметь
// больше этого.
func parseCSSBlocks(css string) []cssBlock {
	var out []cssBlock
	parseCSSBlocksInto(stripCSSCommentsKeepLines(css), 1, "", &out)
	return out
}

// parseCSSBlocksInto — atRule передаёт вниз рекурсии нормализованный текст
// @-правила, внутри которого сейчас идёт разбор ("" на верхнем уровне): при
// входе во вложенный @media/@keyframes рекурсивный вызов получает текст
// ЭТОГО @-правила, а не родительского, — то есть каждому блоку в итоге
// достаётся текст самого БЛИЖАЙШЕГО обёртывающего @-правила, что и нужно
// TestLightThemeBlocksAgree, чтобы отличить @media (prefers-color-scheme:
// light) от любого другого медиа-контекста.
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

		// Ищем парную закрывающую скобку, считая вложенность: обычному
		// правилу это не нужно (внутри объявлений своих "{" не бывает), а
		// @media/@keyframes внутри содержат ровно такие вложенные "{...}".
		depth := 1
		end := openIdx + 1
		for depth > 0 {
			tail := rest[end:]
			nextOpen := strings.IndexByte(tail, '{')
			nextClose := strings.IndexByte(tail, '}')
			if nextClose < 0 {
				// Несбалансированный CSS — защитно останавливаемся, не
				// паникуем: если файл вдруг битый, пусть об этом скажут
				// другие инструменты (gofmt/CI), а не паника внутри стража.
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

// interactiveSelectors — то, у чего граница функциональна, а не декоративна:
// по ней человек понимает, куда кликать (WCAG 1.4.11, порог 3:1).
//
// Список — часть правила, а не список исключений: добавление сюда УЖЕСТОЧАЕТ
// проверку. Именно перечисление трёх штук вместо перебора и оставило
// тринадцать селекторов на декоративном --border (см. debtControlBorderExemptions
// ниже) — ".lang-switcher button" и ".chip"/".tabs" были захардкожены
// литеральными строками в старом TestControlsUseControlBorder
// (internal/web/contrast_test.go), а сама внешняя рамка переключателей
// языка/темы (".lang-switcher, .theme-toggle") — та пара, ради которой токен
// --border-control и вводили — под старые три литерала не попадала вовсе.
var interactiveSelectors = []string{
	"input", "select", "textarea", "button", "summary",
	".chip", ".tab", "[class*=tab]", ".segmented", ".dr-",
	".lang-switcher", ".theme-toggle",
}

// interactiveMarkerRes — по одному скомпилированному регэкспу на маркер из
// interactiveSelectors, посчитанному один раз при загрузке пакета (в цикле
// теста — незачем компилировать заново на каждый блок).
//
// Ведущую точку у маркеров вида ".tab"/".chip" отбрасываем перед сборкой
// регэкспа, а сам поиск идёт по границам слова (\b), а не по голой
// подстроке. Причина — двусторонняя:
//
//   - без отбрасывания точки ".tab" требовал бы точку СРАЗУ перед "tab" и не
//     находил бы .kind-tab (там точка стоит перед "kind", а "tab" приклеен
//     через дефис) — а это ровно один из известных находок аудита: блок
//     .kind-tab держит декоративный --border;
//   - без границы слова голая подстрока "tab" совпала бы и с "table"/
//     .data-table ("table" содержит "tab" как подстроку) — а граница у ячейки
//     таблицы декоративна по своей природе (WCAG 1.4.11 про элементы
//     управления, не про строки таблицы), это был бы ложный, а не просто
//     широкий результат. \b исключает это: между "b" и следующим "l" в
//     "table" нет перехода словообразующий/несловообразующий символ, значит
//     нет границы слова, и "tab" внутри "table" не совпадает — а в
//     "kind-tab" после "b" идёт конец строки/слова, граница есть.
//
// Проверено обоими фактами на реальном app.css (см. task-6-report.md):
// .kind-tab находится, .data-table th/.data-table td — нет.
var interactiveMarkerRes = func() []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(interactiveSelectors))
	for i, m := range interactiveSelectors {
		stripped := strings.TrimPrefix(m, ".")
		res[i] = regexp.MustCompile(`\b` + regexp.QuoteMeta(stripped) + `\b`)
	}
	return res
}()

// selectorIsInteractive — селектор блока УПОМИНАЕТ один из интерактивных
// маркеров (см. interactiveMarkerRes). Само по себе более широкое совпадение
// (например "tab" внутри ".tabs" и ".kind-tab" сразу, не только в
// изначальном ".tab") безопасно шире в СТОРОНУ ужесточения: блок без
// объявления цвета границы просто не пройдёт следующую проверку
// var(--border) и не станет нарушением — в отличие от
// TestNoUndefinedCSSClasses, где широкое совпадение размывало бы РАЗНЫЕ
// имена классов друг в друга и требовало отдельной проверки границы
// (cssClassNameRe), здесь мы не сравниваем множества имён между собой, а
// просто решаем "стоит ли вообще заглянуть в этот блок".
func selectorIsInteractive(selector string) bool {
	for _, re := range interactiveMarkerRes {
		if re.MatchString(selector) {
			return true
		}
	}
	return false
}

// borderColorProps — CSS-свойства, реально определяющие цвет границы.
// border-radius/border-collapse/border-spacing сюда НЕ входят: наивная
// проверка префикса "border" зацепила бы и их, а это другие свойства, не
// имеющие отношения к цвету и контрасту.
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

// lastBorderColorValue возвращает значение ПОСЛЕДНЕГО по тексту объявления
// одного из borderColorProps внутри тела ОДНОГО блока — то, что реально
// "выигрывает", если внутри одного правила одно и то же (или родственное:
// сперва shorthand border, потом уточняющий border-color) свойство
// объявлено дважды. Это и есть суть починки: старый тест смотрел ПЕРВОЕ
// найденное объявление в файле (через strings.Index по всему CSS сразу), а
// не последнее по каскаду для конкретного блока.
//
// Специфичность МЕЖДУ разными блоками (например, два отдельных блока с
// одинаковым селектором ".proj-switch summary" в разных местах файла) эта
// функция не считает — вызывающий код (TestControlsUseControlBorder)
// обходит каждый подходящий блок независимо, что для практики этого файла
// достаточно: ни один найденный интерактивный селектор не переопределяется
// декоративным на --border-control в одном блоке и не наоборот в другом
// (проверено проходом по факту найденных нарушений, см. task-6-report.md).
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

// debtControlBorderExemptions — интерактивные селекторы, у которых
// декоративная граница оставлена осознанно. Долг №32 выжжен подпроектом G;
// список пуст и должен оставаться пустым — новая запись требует явного
// решения с обоснованием.
var debtControlBorderExemptions = []Exemption{}

// maxDebtControlBorderExemptions — потолок долга: 0 после подпроекта G.
const maxDebtControlBorderExemptions = 0

// blocksInsideMedia — сколько блоков parseCSSBlocks нашёл ВНУТРИ какого-либо
// "@media (...)" (не "@keyframes" — у ключевых кадров анимации нет border,
// они этому правилу не интересны, только @media). Единственный потребитель —
// нижний порог minBlocksInsideMedia: сам по себе список найденных блоков
// нигде дальше не используется.
func blocksInsideMedia(blocks []cssBlock) int {
	n := 0
	for _, b := range blocks {
		if strings.HasPrefix(b.AtRule, "@media") {
			n++
		}
	}
	return n
}

// minBlocksInsideMedia — нижний порог для blocksInsideMedia: страховка от
// куда более узкой, но при этом самой опасной регрессии — если
// parseCSSBlocksInto перестанет рекурсировать именно в "@media" (например,
// кто-то в будущем заменит общее условие `strings.HasPrefix(selector, "@")`
// на что-то более узкое и случайно исключит @media), все 17 записей
// debtControlBorderExemptions продолжат находиться как ни в чём не бывало —
// каждая из них уже встречается и ВНЕ @media (см. task про проверку 5,
// final-fix-report.md), поэтому обвал разбора именно в @media-части ни одна
// из них не заметит.
//
// Порог — фактическое число (проверено на этом app.css: 33 блока внутри
// тринадцати разных @media, от `prefers-color-scheme` до `min-width`), с запасом
// вниз тем же приёмом, что у TestHelpPanelKeysResolve (< 10 при фактических
// 17) и TestMonitorErrorCodesResolve (< 20 при фактических 27) в
// i18n_dynamic_test.go — округлённое число заметно ниже факта, чтобы порог
// ловил обвал самого разбора, а не обычные колебания состава стилей при
// правках вёрстки.
const minBlocksInsideMedia = 20

// TestControlsUseControlBorder: правило с большей специфичностью не должно
// возвращать интерактивному элементу декоративную границу.
//
// Так и было: --border-control завели под WCAG 1.4.11, а фильтры проблем,
// вкладки, чипы и переключатели языка/темы продолжали брать --border —
// измерено 1.40:1 в тёмной теме. Старая версия этого теста
// (internal/web/contrast_test.go) смотрела три литеральных селектора и
// первое их вхождение в файле — и то, и другое пряталось: перечисление трёх
// строк вместо полного перебора не находило остальные места (см.
// debtControlBorderExemptions — их набралось 17), а поиск первого вхождения
// не разглядел бы более позднее (и потому побеждающее в каскаде)
// переопределение того же селектора — см. мутационную проверку в
// task-6-report.md: дописанный в конец файла ".card .chip
// {border-color:var(--border)}" находится этим правилом именно потому, что
// оно смотрит ВСЕ блоки, а не первый попавшийся.
func TestControlsUseControlBorder(t *testing.T) {
	tree := Load(t)
	blocks := parseCSSBlocks(tree.CSS.Body)

	// Долговой список ниже не поймает регрессию разбора @media сам по себе
	// (все 17 его записей находятся и вне @media) — этот порог ловит именно
	// её: см. комментарий у minBlocksInsideMedia.
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
