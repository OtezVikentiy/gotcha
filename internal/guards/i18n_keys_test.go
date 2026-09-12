package guards

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// [^,]+? не пройдёт через запятую внутри вложенных скобок первого аргумента
// (i18n.T(withLocale(ctx, loc), "ключ")) — такой вызов сканер молча пропустит.
var literalKeyRe = regexp.MustCompile(`i18n\.(T|Tf|Tn)\(\s*[^,]+?\s*,\s*"([^"]+)"(\s*\+)?`)

const minKeysFound = 730

// T/Tf ищут ключ в Catalogs, Tn — в Plurals; ключ, вызванный не той функцией,
// на странице отдаст сырой ключ так же, как и полное отсутствие перевода.
func TestEveryKeyInCodeExistsInCatalog(t *testing.T) {
	tree := Load(t)

	messageKeys, pluralKeys := collectI18nKeys(t, tree)
	total := len(messageKeys) + len(pluralKeys)
	if total < minKeysFound {
		t.Fatalf("сканер нашёл %d ключей (%d обычных + %d плюральных), ожидалось не меньше %d — это регрессия самого сканера, а не каталога",
			total, len(messageKeys), len(pluralKeys), minKeysFound)
	}

	var missing []string
	for k := range messageKeys {
		if _, ok := tree.Catalogs["ru"][k]; !ok {
			missing = append(missing, fmt.Sprintf("%s (i18n.T/Tf, нет в messages)", k))
		}
	}
	for k := range pluralKeys {
		if _, ok := tree.Plurals["ru"][k]; !ok {
			missing = append(missing, fmt.Sprintf("%s (i18n.Tn, нет в plurals)", k))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		// Парность ru/en каталогов проверяет TestCatalogsHaveIdenticalKeys
		// (internal/i18n/catalog_test.go) — здесь достаточно одной локали.
		t.Errorf("ключи используются в коде, но отсутствуют в каталоге (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

func collectI18nKeys(t *testing.T, tree *Tree) (messageKeys, pluralKeys map[string]bool) {
	t.Helper()
	messageKeys = map[string]bool{}
	pluralKeys = map[string]bool{}

	scan := func(body string) {
		body = stripCommentsPerLine(body)
		for _, m := range literalKeyRe.FindAllStringSubmatch(body, -1) {
			// Конкатенация ("range." + ключ) — не литеральный ключ, множество
			// значений проверяют отдельные тесты рядом с местом сборки.
			if strings.TrimSpace(m[3]) == "+" {
				continue
			}
			if m[1] == "Tn" {
				pluralKeys[m[2]] = true
			} else {
				messageKeys[m[2]] = true
			}
		}
	}
	for _, f := range tree.GoFiles {
		// _templ.go дублирует находки своего .templ-исходника, сканируемого ниже.
		if f.Generated {
			continue
		}
		// Тест вправе намеренно звать несуществующий ключ, проверяя поведение
		// продукта при дыре в каталоге — это часть проверки, а не находка.
		if strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		scan(f.Body)
	}
	for _, f := range tree.Templates {
		scan(f.Body)
	}
	return messageKeys, pluralKeys
}

// stripTrailingComment ищет "//" не по первому вхождению, а по первому с
// чётным числом кавычек перед ним — иначе резало бы на "//" внутри URL-литерала.
func stripCommentsPerLine(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		lines[i] = stripTrailingComment(line)
	}
	return strings.Join(lines, "\n")
}
