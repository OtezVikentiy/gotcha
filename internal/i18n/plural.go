package i18n

import (
	"context"
	"strconv"
	"strings"
)

func Tn(ctx context.Context, key string, n int) string {
	code := FromContext(ctx).Code
	s := pluralLookup(code, key, pluralForm(code, n))
	return strings.ReplaceAll(s, "{n}", strconv.Itoa(n))
}

// Тот же контракт, что у lookup — рендер не падает, промах учитывается через recordMissingKey.
func pluralLookup(code, key, form string) string {
	if v, ok := pluralLookupOwn(code, key, form); ok {
		return v
	}
	if code != Default.Code {
		if v, ok := pluralLookupOwn(Default.Code, key, form); ok {
			recordMissingKey(code, key, MissingKeyFallback)
			return v
		}
	}
	recordMissingKey(code, key, MissingKeyMissing)
	return key
}

func pluralLookupOwn(code, key, form string) (string, bool) {
	c, ok := catalogs[code]
	if !ok {
		return "", false
	}
	forms, ok := c.Plurals[key]
	if !ok {
		return "", false
	}
	if v, ok := forms[form]; ok {
		return v, true
	}
	if v, ok := forms["other"]; ok {
		return v, true
	}
	return "", false
}

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
