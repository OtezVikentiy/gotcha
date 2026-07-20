package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var cyrillicLiteral = regexp.MustCompile(`"[^"]*[а-яА-Я][^"]*"`)

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
			case strings.Contains(line, "log."), strings.Contains(line, "slog."):
				continue
			case !cyrillicLiteral.MatchString(line):
				continue
			}
			t.Errorf("%s:%d: русский литерал вне каталога i18n: %s", f, i+1, trimmed)
		}
	}
}
