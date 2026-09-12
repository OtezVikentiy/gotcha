package guards

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode"
)

var errorStyleFamilies = []string{"err.", "error.", "flash."}

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
