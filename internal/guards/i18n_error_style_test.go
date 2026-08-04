package guards

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// errorStyleFamilies — семейства ключей, к которым применяется канон
// сообщений об ошибках и flash (№67).
var errorStyleFamilies = []string{"err.", "error.", "flash."}

// TestErrorMessageStyle — канон пользовательских сообщений об ошибках и
// flash (№67): первая БУКВА — в верхнем регистре (для ru — кириллическая
// заглавная), последний символ — не '.', '…' и не '!': плашка — не
// предложение в абзаце и не окрик. На момент аудита 112 значений err.*/
// error.* были разнобойными (68 со строчной, 44 с заглавной, часть с
// точкой) — выглядит небрежностью и мешает читать плашки как класс.
//
// Проверяются ОБА каталога. Значение может начинаться с {подстановки},
// цифры или кавычки — тогда проверяется первая буква после них; значение
// вовсе без букв (теоретическое) пропускается.
func TestErrorMessageStyle(t *testing.T) {
	tree := Load(t)

	var bad []string
	for _, lang := range []string{"ru", "en"} {
		keys := make([]string, 0, len(tree.Catalogs[lang]))
		for k := range tree.Catalogs[lang] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			inFamily := false
			for _, fam := range errorStyleFamilies {
				if strings.HasPrefix(k, fam) {
					inFamily = true
					break
				}
			}
			if !inFamily {
				continue
			}
			v := tree.Catalogs[lang][k]
			if v == "" {
				continue
			}
			if r, ok := firstLetter(v); ok && !unicode.IsUpper(r) {
				bad = append(bad, fmt.Sprintf("%s[%s] = %q: первая буква строчная", lang, k, v))
			}
			runes := []rune(v)
			switch last := runes[len(runes)-1]; last {
			case '.', '…', '!':
				bad = append(bad, fmt.Sprintf("%s[%s] = %q: финальный %q", lang, k, v, string(last)))
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("канон сообщений об ошибках (№67: с заглавной, без финальной точки) нарушен %d раз:\n%s",
			len(bad), strings.Join(bad, "\n"))
	}
}

// firstLetter — первая буква строки, пропуская подстановки {name}, цифры,
// кавычки и прочие небуквенные символы. ok=false — букв нет вовсе.
func firstLetter(s string) (rune, bool) {
	inPlaceholder := false
	for _, r := range s {
		switch {
		case r == '{':
			inPlaceholder = true
		case r == '}':
			inPlaceholder = false
		case !inPlaceholder && unicode.IsLetter(r):
			return r, true
		}
	}
	return 0, false
}
