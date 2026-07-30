package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Диапазон [а-яА-Я] не включает «ё» и «Ё»: они стоят в Unicode отдельно, вне
// диапазона а-я. Литерал «Не найдено» ловился, а «Ключ ещё не создан» — нет.
var cyrillicLiteral = regexp.MustCompile(`"[^"]*[а-яА-ЯёЁ][^"]*"`)

// TestNoCyrillicUserFacingLiterals — user-facing текст должен жить в каталоге
// i18n, а не в Go-коде. Русская строка в хендлере не ломает ни сборку, ни
// тесты — она просто показывается английскому посетителю как есть.
//
// Исключения по строке: комментарии (в проекте они русские) и аргументы
// логгера — тексты для оператора, а не для посетителя, их язык привязан к
// языку кодовой базы, а не к локали запроса.
func TestNoCyrillicUserFacingLiterals(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("не найдено ни одного .go — проверь рабочую директорию теста")
	}

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, "_templ.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "//"):
				continue
			case isLogCall(line):
				continue
			case !cyrillicLiteral.MatchString(line):
				continue
			}
			t.Errorf("%s:%d: русский литерал вне каталога i18n: %s", f, i+1, trimmed)
		}
	}
}

// isLogCall — строка ли это вызова журналирования.
//
// Проверка по подстроке «log.» совпадала и с «catalog.», и с «dialog.», и с
// любым идентификатором, заканчивающимся на log: страж молча пропускал бы
// русский литерал в такой строке. Совпадение должно начинаться на границе
// идентификатора.
func isLogCall(line string) bool {
	return logCallRe.MatchString(line)
}

var logCallRe = regexp.MustCompile(`(^|[^\w.])s?log\.`)
