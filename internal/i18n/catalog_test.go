package i18n

import (
	"regexp"
	"strings"
	"testing"
)

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

// Долг: ключ/локаль несёт форму, которую pluralForm для неё никогда не вернёт.
// Новых записей не добавлять — либо форма нужна (чинить pluralForm), либо мертва (чинить каталог).
var deadPluralFormDebt = map[string]map[string][]string{
	"ru": {
		"chart.bar.transactions":       {"other"},
		"chart.bar.events":             {"other"},
		"issue.times_seen":             {"other"},
		"time.ago.seconds":             {"other"},
		"time.ago.minutes":             {"other"},
		"time.ago.hours":               {"other"},
		"time.ago.days":                {"other"},
		"org.quota.dropped_banner":     {"other"},
		"org.gdpr.purge.result":        {"other"},
		"cardinality.notice.collapsed": {"other"},
		"metrics.system.show_toggle":   {"other"},
		"flash.issues_resolved":        {"other"},
		"flash.issues_ignored":         {"other"},
		"flash.issues_reopened":        {"other"},
		"flash.subject_purged":         {"other"},
		"unit.minutes":                 {"other"},
		"unit.hours":                   {"other"},
		"unit.days":                    {"other"},
		"unit.seconds":                 {"other"},
		"exports.mail.done.rows":       {"other"},
	},
	"en": {
		"issue.times_seen": {"few", "many"},
	},
}

// TestCatalogsHaveIdenticalKeys сверяет наборы ключей между локалями, не наборы форм
// внутри ключа — этот тест ловит форму, недостижимую в рантайме, кроме учтённого долга.
func TestNoUnreachablePluralForms(t *testing.T) {
	for code, cat := range catalogs {
		reachable := reachablePluralForms(code)
		debt := deadPluralFormDebt[code]
		for key, forms := range cat.Plurals {
			for form := range forms {
				if reachable[form] {
					continue
				}
				dead := debt[key]
				found := false
				for _, d := range dead {
					if d == form {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s.json: плюрал %q несёт недостижимую форму %q (не в deadPluralFormDebt)", code, key, form)
				}
			}
		}
	}
}

func TestNoEmptyMessages(t *testing.T) {
	for code, c := range catalogs {
		for k, v := range c.Messages {
			if v == "" {
				t.Errorf("%s.json: у ключа %q пустое значение", code, k)
			}
		}
	}
}

func TestCatalogsUsePlaceholderSyntax(t *testing.T) {
	// %% экранированный — не подстановка, вырезаем перед матчем.
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
			continue
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
