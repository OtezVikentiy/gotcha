package guards

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	iconsTemplPath        = "internal/web/templates/icons.templ"
	emptyStateTemplPath   = "internal/web/templates/emptystate.templ"
	layoutTemplPath       = "internal/web/templates/layout.templ"
	dependenciesTemplPath = "internal/web/templates/dependencies.templ"
	navGoPath             = "internal/nav/nav.go"
	daterangeJSPath       = "internal/web/static/daterange.js"
)

var spriteSymbolRe = regexp.MustCompile(`<symbol id="i-([a-z0-9-]+)"`)

func spriteSymbols(iconsTempl string) map[string]bool {
	out := map[string]bool{}
	for _, m := range spriteSymbolRe.FindAllStringSubmatch(iconsTempl, -1) {
		out[m[1]] = true
	}
	return out
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// findCalls ищет вызовы "marker(" в body и возвращает СЫРОЙ текст аргументов
// каждого (с учётом вложенных скобок). exclude — байты непосредственно перед
// вхождением, при которых совпадение отбрасывается (например, объявление функции).
func findCalls(body, marker string, exclude ...string) []string {
	var calls []string
	search := 0
	for {
		idx := strings.Index(body[search:], marker+"(")
		if idx < 0 {
			break
		}
		start := search + idx
		if start > 0 && (body[start-1] == '.' || isIdentByte(body[start-1])) {
			search = start + len(marker)
			continue
		}
		skip := false
		for _, ex := range exclude {
			if start >= len(ex) && body[start-len(ex):start] == ex {
				skip = true
			}
		}
		argStart := start + len(marker) + 1
		depth := 1
		i := argStart
		for ; i < len(body) && depth > 0; i++ {
			switch body[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		if !skip {
			calls = append(calls, strings.TrimSpace(body[argStart:i-1]))
		}
		search = i
	}
	return calls
}

// splitTopLevelArgs делит аргументы вызова по запятым верхнего уровня — не
// внутри кавычек и не внутри вложенных скобок.
func splitTopLevelArgs(s string) []string {
	var out []string
	depth := 0
	inStr := false
	last := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inStr = !inStr
		case '(':
			if !inStr {
				depth++
			}
		case ')':
			if !inStr {
				depth--
			}
		case ',':
			if !inStr && depth == 0 {
				out = append(out, strings.TrimSpace(s[last:i]))
				last = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[last:]))
	return out
}

func stringLiteral(expr string) (string, bool) {
	if len(expr) >= 2 && expr[0] == '"' && expr[len(expr)-1] == '"' {
		return expr[1 : len(expr)-1], true
	}
	return "", false
}

// requestedIconNames — обе стороны сторожа считают по ОДНОМУ и тому же множеству,
// собранному здесь. Динамические источники не перечисляются числами вручную —
// каждый выводится из своих же call sites или тела функции, чтобы не расходиться
// с кодом при следующей правке. Всё, что не литерал и не входит в этот закрытый
// список источников, роняет тест — а не пропускается.
func requestedIconNames(t *testing.T, tree *Tree) map[string]bool {
	t.Helper()
	requested := map[string]bool{}

	var allTempl strings.Builder
	fileByPath := map[string]string{}
	for _, f := range tree.Templates {
		allTempl.WriteString(f.Body)
		allTempl.WriteString("\n")
		fileByPath[f.Path] = f.Body
	}
	combined := allTempl.String()

	// Источник 1: emptyState(iconName, ...) — сам iconName берёт значение из
	// первого литерала КАЖДОГО вызова @emptyState(...) по всему дереву шаблонов.
	emptyStateNames := map[string]bool{}
	for _, args := range findCalls(combined, "@emptyState") {
		parts := splitTopLevelArgs(args)
		if len(parts) == 0 {
			continue
		}
		lit, ok := stringLiteral(parts[0])
		if !ok {
			t.Fatalf("@emptyState(...) с нелитеральным первым аргументом %q — сторож спрайта не может перечислить возможные имена", parts[0])
		}
		emptyStateNames[lit] = true
	}

	// Источник 2: themeBtn(code, iconName, labelKey) — iconName берёт значение
	// из второго литерала каждого вызова @themeBtn(...).
	themeBtnNames := map[string]bool{}
	for _, args := range findCalls(combined, "@themeBtn") {
		parts := splitTopLevelArgs(args)
		if len(parts) < 2 {
			t.Fatalf("@themeBtn(%s) — ожидалось минимум 2 аргумента", args)
		}
		lit, ok := stringLiteral(parts[1])
		if !ok {
			t.Fatalf("@themeBtn(...) с нелитеральным вторым аргументом %q", parts[1])
		}
		themeBtnNames[lit] = true
	}

	// Источник 3: depDirectionIcon(dir) — перечисление разбирается из тела самой
	// функции в dependencies.templ, а не переписывается вручную здесь.
	depBody, ok := fileByPath[dependenciesTemplPath]
	if !ok {
		t.Fatalf("не нашёл %s в дереве шаблонов", dependenciesTemplPath)
	}
	depFuncStart := strings.Index(depBody, "func depDirectionIcon(")
	if depFuncStart < 0 {
		t.Fatalf("%s: функция depDirectionIcon не найдена — источник @icon(depDirectionIcon(...)) не проверить", dependenciesTemplPath)
	}
	depFuncEnd := strings.Index(depBody[depFuncStart:], "\n}\n")
	if depFuncEnd < 0 {
		t.Fatalf("%s: не нашёл конец тела depDirectionIcon", dependenciesTemplPath)
	}
	depReturnRe := regexp.MustCompile(`return "([a-z0-9-]+)"`)
	depDirectionNames := map[string]bool{}
	for _, m := range depReturnRe.FindAllStringSubmatch(depBody[depFuncStart:depFuncStart+depFuncEnd], -1) {
		depDirectionNames[m[1]] = true
	}
	if len(depDirectionNames) == 0 {
		t.Fatalf("%s: depDirectionIcon не вернул ни одного литерала — регэксп мог разойтись с кодом", dependenciesTemplPath)
	}

	// Источник 4: nav.IconName — прямые литералы Areas() плюс поле icon
	// строчек railAreas (тот же порядок полей: id, icon, labelKey, footer).
	var navBody string
	for _, f := range tree.GoFiles {
		if f.Path == navGoPath {
			navBody = f.Body
		}
	}
	if navBody == "" {
		t.Fatalf("не нашёл %s в дереве Go-файлов", navGoPath)
	}
	navIconNames := map[string]bool{}
	for _, m := range regexp.MustCompile(`IconName:\s*"([a-z0-9-]+)"`).FindAllStringSubmatch(navBody, -1) {
		navIconNames[m[1]] = true
	}
	railStart := strings.Index(navBody, "railAreas = []struct")
	if railStart < 0 {
		t.Fatalf("%s: не нашёл railAreas — источник a.IconName не проверить", navGoPath)
	}
	railEnd := strings.Index(navBody[railStart:], "\n}\n")
	if railEnd < 0 {
		t.Fatalf("%s: не нашёл конец блока railAreas", navGoPath)
	}
	railRowRe := regexp.MustCompile(`\{"[^"]*",\s*"([a-z0-9-]+)",\s*"[^"]*",\s*(?:true|false)\}`)
	for _, m := range railRowRe.FindAllStringSubmatch(navBody[railStart:railStart+railEnd], -1) {
		navIconNames[m[1]] = true
	}
	if len(navIconNames) == 0 {
		t.Fatalf("%s: не нашёл ни одного имени иконки в railAreas/IconName — регэксп мог разойтись с кодом", navGoPath)
	}

	// Закрытый реестр: ключ — "путь::выражение-аргумента", как оно ДОСЛОВНО
	// встречается в @icon(...). Неизвестный ключ ниже — падение, не пропуск.
	registry := map[string]map[string]bool{
		emptyStateTemplPath + "::iconName":                        emptyStateNames,
		layoutTemplPath + "::iconName":                            themeBtnNames,
		layoutTemplPath + "::a.IconName":                          navIconNames,
		dependenciesTemplPath + "::depDirectionIcon(d.Direction)": depDirectionNames,
	}

	for _, f := range tree.Templates {
		for _, args := range findCalls(f.Body, "@icon") {
			if lit, ok := stringLiteral(args); ok {
				requested[lit] = true
				continue
			}
			names, ok := registry[f.Path+"::"+args]
			if !ok {
				t.Fatalf("%s: @icon(%s) — аргумент не литерал и не зарегистрирован в реестре динамических источников сторожа спрайта; добавь источник явно или замени литералом", f.Path, args)
				continue
			}
			for n := range names {
				requested[n] = true
			}
		}
	}
	for name := range emptyStateNames {
		requested[name] = true
	}

	// Источник 5: клиентский сборщик icon(name) в daterange.js — те же правила:
	// литерал берётся как есть, что угодно другое обязано упасть.
	jsBody, err := os.ReadFile(filepath.Join(tree.Root, daterangeJSPath))
	if err != nil {
		t.Fatalf("чтение %s: %v", daterangeJSPath, err)
	}
	for _, args := range findCalls(string(jsBody), "icon", "function ") {
		lit, ok := stringLiteral(args)
		if !ok {
			t.Fatalf("%s: icon(%s) — аргумент не строковый литерал; сторож спрайта не умеет проверить динамическое имя из JS", daterangeJSPath, args)
			continue
		}
		requested[lit] = true
	}

	return requested
}

// Каждое запрашиваемое где-либо имя иконки обязано существовать в спрайте —
// иначе <use href="#i-..."> ссылается в пустоту и иконка пропадает молча.
func TestIconSpriteContainsEveryRequestedName(t *testing.T) {
	tree := Load(t)
	var iconsBody string
	for _, f := range tree.Templates {
		if f.Path == iconsTemplPath {
			iconsBody = f.Body
		}
	}
	if iconsBody == "" {
		t.Fatalf("не нашёл %s в дереве шаблонов", iconsTemplPath)
	}
	sprite := spriteSymbols(iconsBody)
	requested := requestedIconNames(t, tree)

	for name := range requested {
		if !sprite[name] {
			t.Errorf("иконка %q запрашивается, но символа i-%s нет в спрайте (%s)", name, name, iconsTemplPath)
		}
	}
}

// Символ спрайта, который никто не запрашивает, — мёртвый вес в каждом ответе.
// Храповик на нуле: список исключений не заводится, потому что исключать нечего.
func TestIconSpriteHasNoOrphanSymbols(t *testing.T) {
	tree := Load(t)
	var iconsBody string
	for _, f := range tree.Templates {
		if f.Path == iconsTemplPath {
			iconsBody = f.Body
		}
	}
	if iconsBody == "" {
		t.Fatalf("не нашёл %s в дереве шаблонов", iconsTemplPath)
	}
	sprite := spriteSymbols(iconsBody)
	requested := requestedIconNames(t, tree)

	for name := range sprite {
		if !requested[name] {
			t.Errorf("символ i-%s есть в спрайте, но его никто не запрашивает — мёртвый вес в каждом ответе, убери его из %s", name, iconsTemplPath)
		}
	}
}
