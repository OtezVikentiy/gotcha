package i18n

import (
	"context"
	"strconv"
	"strings"
)

// Tn — перевод с множественным числом: выбирает форму (one/few/many/other) по
// числу и локали (CLDR-правила ru/en) и подставляет {n}.
func Tn(ctx context.Context, key string, n int) string {
	code := FromContext(ctx).Code
	s := pluralLookup(code, key, pluralForm(code, n))
	return strings.ReplaceAll(s, "{n}", strconv.Itoa(n))
}

func pluralLookup(code, key, form string) string {
	if c, ok := catalogs[code]; ok {
		if forms, ok := c.Plurals[key]; ok {
			if v, ok := forms[form]; ok {
				return v
			}
			if v, ok := forms["other"]; ok {
				return v
			}
		}
	}
	if code != Default.Code {
		return pluralLookup(Default.Code, key, form)
	}
	return key
}

// pluralForm — CLDR-категория количественного числа для локали.
func pluralForm(code string, n int) string {
	if n < 0 {
		n = -n
	}
	if code == "ru" {
		mod10, mod100 := n%10, n%100
		switch {
		case mod10 == 1 && mod100 != 11:
			return "one"
		case mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14):
			return "few"
		default:
			return "many"
		}
	}
	if n == 1 {
		return "one"
	}
	return "other"
}
