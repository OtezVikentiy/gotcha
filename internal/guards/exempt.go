package guards

import (
	"regexp"
	"strings"
)

type Exemption struct {
	Value   string
	Why     string
	Finding string
}

// Узкий интерфейс вместо *testing.T — чтобы храповик можно было проверить самим тестом.
type testingT interface {
	Helper()
	Errorf(format string, args ...any)
}

func ExemptedValues(list []Exemption) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, e := range list {
		out[e.Value] = true
	}
	return out
}

// Три нарушения: запись без Why, список длиннее max, и запись на значение,
// которого больше нет в seen (устаревшее исключение — храповик).
func CheckExemptions(t testingT, name string, list []Exemption, max int, seen map[string]bool) {
	t.Helper()

	for _, e := range list {
		if e.Why == "" {
			t.Errorf("%s: исключение %q (%s) без причины — заполните Why", name, e.Value, e.Finding)
		}
		if !seen[e.Value] {
			t.Errorf("%s: исключение %q (%s) устарело — нарушение больше не найдено, строку надо удалить из списка исключений",
				name, e.Value, e.Finding)
		}
	}
	if len(list) > max {
		t.Errorf("%s: список исключений разросся до %d записей при потолке %d — почините находки или осознанно поднимите потолок",
			name, len(list), max)
	}
}

// Регэксп, не go/ast: находит только НАЗВАННЫЕ func/templ с нулевой колонки —
// вложенные замыкания и `var x = func()` не матчатся, генерики учтены `(?:\[[^\]]*\])?`.
var funcDeclRe = regexp.MustCompile(`^(?:func|templ)\s+(?:\(\s*\S+\s+\*?(\w+)\s*\)\s+)?(\w+)(?:\[[^\]]*\])?\s*\(`)

// Ресивер вплетается в имя как "Тип.Метод" — иначе два одноимённых метода на
// разных ресиверах схлопнутся в одно имя.
func funcContexts(body string) []string {
	lines := strings.Split(body, "\n")
	ctx := make([]string, len(lines))
	current := ""
	for i, line := range lines {
		if m := funcDeclRe.FindStringSubmatch(line); m != nil {
			if m[1] != "" {
				current = m[1] + "." + m[2]
			} else {
				current = m[2]
			}
		}
		ctx[i] = current
	}
	return ctx
}

// Ключ не включает номер строки — правка выше находки не сдвигает и не
// портит его; переименование функции или правка самой строки якорь всё же меняют.
func ContentAnchor(path, funcName, line string) string {
	if funcName == "" {
		// Латиницей: exempt.go сам входит в обход TestNoCyrillicUserFacingLiterals.
		funcName = "(top level)"
	}
	return path + " in " + funcName + ": " + line
}

// Тот же якорь на разных строках — неоднозначность, тест обязан упасть;
// на одной и той же строке (два вызова подряд) — ожидаемо, не ошибка.
func recordAnchor(t testingT, name string, seenLines map[string]int, anchor string, line int) {
	t.Helper()
	if prev, ok := seenLines[anchor]; ok {
		if prev != line {
			t.Errorf("%s: якорь %q неоднозначен — совпадает и со строкой %d, и со строкой %d одновременно; нужен более точный контекст (другое имя функции или различающийся фрагмент строки), а не молчаливый выбор одной из них",
				name, anchor, prev, line)
		}
		return
	}
	seenLines[anchor] = line
}
