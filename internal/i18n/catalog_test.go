package i18n

import (
	"regexp"
	"strings"
	"testing"
)

// TestCatalogsHaveIdenticalKeys — страж парности каталогов. Ключ, добавленный
// только в один язык, не ломает ни сборку, ни существующие тесты: lookup
// молча падает в Default, а оттуда в сам ключ. То есть английская страница
// показывает русский текст (или сырой "nav.issues"), и заметить это можно
// только глазами на визуальной приёмке. Ловим тестом.
func TestCatalogsHaveIdenticalKeys(t *testing.T) {
	ru, en := catalogs["ru"], catalogs["en"]

	for k := range ru.Messages {
		if _, ok := en.Messages[k]; !ok {
			t.Errorf("messages: ключ %q есть в ru.json, но отсутствует в en.json", k)
		}
	}
	for k := range en.Messages {
		if _, ok := ru.Messages[k]; !ok {
			t.Errorf("messages: ключ %q есть в en.json, но отсутствует в ru.json", k)
		}
	}
	for k := range ru.Plurals {
		if _, ok := en.Plurals[k]; !ok {
			t.Errorf("plurals: ключ %q есть в ru.json, но отсутствует в en.json", k)
		}
	}
	for k := range en.Plurals {
		if _, ok := ru.Plurals[k]; !ok {
			t.Errorf("plurals: ключ %q есть в en.json, но отсутствует в ru.json", k)
		}
	}
}

// TestPluralFormsComplete — недостающая форма множественного числа не даёт
// ошибки: pluralForm выбирает категорию, её нет в JSON, и текст схлопывается
// в other. По-русски это выглядит как «5 проблема».
func TestPluralFormsComplete(t *testing.T) {
	required := map[string][]string{
		"ru": {"one", "few", "many"},
		"en": {"one", "other"},
	}
	for code, forms := range required {
		for key, got := range catalogs[code].Plurals {
			for _, f := range forms {
				if got[f] == "" {
					t.Errorf("%s.json: у плюрала %q нет формы %q", code, key, f)
				}
			}
		}
	}
}

// TestNoEmptyMessages — пустое значение ключа выглядит как «строка пропала»
// и неотличимо от бага вёрстки.
func TestNoEmptyMessages(t *testing.T) {
	for code, c := range catalogs {
		for k, v := range c.Messages {
			if v == "" {
				t.Errorf("%s.json: у ключа %q пустое значение", code, k)
			}
		}
	}
}

// TestCatalogsUsePlaceholderSyntax — страж синтаксиса подстановок. Tf
// подставляет только {name}, а привычка писать %s ничего не ломает: строка
// собирается, тесты проходят, и на странице остаётся буквальное «Тип канала:
// %s.». Поймано ровно так — глазами на приёмке; ловим тестом.
func TestCatalogsUsePlaceholderSyntax(t *testing.T) {
	// %% — экранированный процент, он к подстановкам отношения не имеет.
	printfVerb := regexp.MustCompile(`%[sdvqft]`)
	for loc, cat := range catalogs {
		for k, v := range cat.Messages {
			if printfVerb.MatchString(strings.ReplaceAll(v, "%%", "")) {
				t.Errorf("%s: %q содержит printf-подстановку (%q); Tf понимает только {name}", loc, k, v)
			}
		}
		for k, forms := range cat.Plurals {
			for form, v := range forms {
				if printfVerb.MatchString(strings.ReplaceAll(v, "%%", "")) {
					t.Errorf("%s: %q/%s содержит printf-подстановку (%q); Tn понимает только {n}", loc, k, form, v)
				}
			}
		}
	}
}

// TestCatalogPlaceholdersMatchAcrossLocales — набор подстановок в переводе
// должен совпадать с оригиналом. Пропущенный {slug} в одном языке — это
// предложение, где на месте значения ничего нет; лишний — буквальные фигурные
// скобки на странице.
func TestCatalogPlaceholdersMatchAcrossLocales(t *testing.T) {
	placeholder := regexp.MustCompile(`\{[a-z_]+\}`)
	names := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, m := range placeholder.FindAllString(s, -1) {
			out[m] = true
		}
		return out
	}
	ru, en := catalogs["ru"], catalogs["en"]
	for k, ruText := range ru.Messages {
		enText, ok := en.Messages[k]
		if !ok {
			continue // парность ключей проверяет соседний тест
		}
		want, got := names(ruText), names(enText)
		if len(want) != len(got) {
			t.Errorf("%q: подстановки расходятся — ru %v, en %v", k, want, got)
			continue
		}
		for name := range want {
			if !got[name] {
				t.Errorf("%q: подстановки %s нет в en.json", k, name)
			}
		}
	}
}
