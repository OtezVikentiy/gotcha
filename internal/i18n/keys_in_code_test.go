package i18n_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// literalKeyRe находит вызовы перевода с ЛИТЕРАЛЬНЫМ ключом:
// i18n.T(ctx, "issues.title"), i18n.Tf(...), i18n.Tn(...).
//
// Динамически собранные ключи (конкатенация) сюда не попадают намеренно — их
// закрепляют отдельные тесты рядом с местом сборки, потому что множество
// значений знает только вызывающий.
var literalKeyRe = regexp.MustCompile(`i18n\.(T|Tf|Tn)\(\s*ctx\s*,\s*"([^"]+)"(\s*\+)?`)

// TestEveryKeyInCodeExistsInCatalog — ключ, которого нет ни в одном каталоге,
// не ловило НИЧТО.
//
// Рендер-тесты шаблонов проверяют только те значения, которые сами и
// подставили, поэтому страница с сырыми ключами вместо заголовков колонок
// проходила весь набор: перевода нет, ошибки нет, на экране «issues.table.level».
func TestEveryKeyInCodeExistsInCatalog(t *testing.T) {
	keys := collectLiteralKeys(t)
	if len(keys) < 100 {
		t.Fatalf("найдено %d ключей — сканер сломан, проверять нечего", len(keys))
	}

	var missing []string
	for _, k := range keys {
		// T и Tn возвращают сам ключ, когда перевода нет: это и есть признак
		// дыры. Плюральные ключи живут в отдельном разделе каталога, поэтому
		// проверяются через Tn.
		if i18n.T(t.Context(), k.name) == k.name && i18n.Tn(t.Context(), k.name, 1) == k.name {
			missing = append(missing, k.name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("ключи используются в коде, но отсутствуют в каталогах (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// literalKey — ключ, найденный в исходниках.
type literalKey struct{ name string }

// collectLiteralKeys обходит исходники и собирает литеральные ключи.
func collectLiteralKeys(t *testing.T) []literalKey {
	t.Helper()
	seen := map[string]bool{}
	roots := []string{"../../internal", "../../cmd"}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			// Сгенерированные шаблоны пропускаем: их ключи уже есть в .templ,
			// а дубли только замедляют проверку.
			if strings.HasSuffix(name, "_templ.go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".templ") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range literalKeyRe.FindAllStringSubmatch(stripComments(string(data)), -1) {
				// Конкатенация («range.» + ключ) — не литеральный ключ, а
				// префикс: множество его значений знает только вызывающий, и
				// закрепляют его отдельные тесты рядом с местом сборки.
				if strings.TrimSpace(m[3]) == "+" {
					continue
				}
				seen[m[2]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("обход %s: %v", root, err)
		}
	}
	names := make([]string, 0, len(seen))
	for k := range seen {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]literalKey, 0, len(names))
	for _, n := range names {
		out = append(out, literalKey{name: n})
	}
	return out
}

// stripComments убирает строчные комментарии: в них встречаются примеры вызовов
// («обычно i18n.T(ctx, "nav.…")»), и сканер принимал их за настоящие ключи.
//
// Достаточно строчных: блочные в этом коде не используются для примеров, а
// полноценный разбор Go и templ ради теста — цена выше пользы.
func stripComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if j := strings.Index(line, "//"); j >= 0 {
			// Кавычка перед «//» означает, что это часть строки, а не
			// комментарий (например URL в литерале).
			if strings.Count(line[:j], `"`)%2 == 0 {
				lines[i] = line[:j]
			}
		}
	}
	return strings.Join(lines, "\n")
}
