package i18n

import "testing"

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
