package i18n

import (
	"context"
	"testing"
)

func TestPluralFormRU(t *testing.T) {
	cases := map[int]string{1: "one", 2: "few", 3: "few", 4: "few", 5: "many",
		11: "many", 12: "many", 14: "many", 21: "one", 22: "few", 25: "many", 0: "many"}
	for n, want := range cases {
		if got := pluralForm("ru", n); got != want {
			t.Fatalf("pluralForm(ru,%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPluralFormEN(t *testing.T) {
	if pluralForm("en", 1) != "one" || pluralForm("en", 0) != "other" || pluralForm("en", 5) != "other" {
		t.Fatal("en plural forms wrong")
	}
}

func TestTn(t *testing.T) {
	ru := WithLocale(context.Background(), Locale{Code: "ru"})
	if got := Tn(ru, "issue.times_seen", 1); got != "1 раз" {
		t.Fatalf("Tn ru 1 = %q", got)
	}
	if got := Tn(ru, "issue.times_seen", 2); got != "2 раза" {
		t.Fatalf("Tn ru 2 = %q", got)
	}
	if got := Tn(ru, "issue.times_seen", 5); got != "5 раз" {
		t.Fatalf("Tn ru 5 = %q", got)
	}
	en := WithLocale(context.Background(), Locale{Code: "en"})
	if got := Tn(en, "issue.times_seen", 3); got != "3 times" {
		t.Fatalf("Tn en 3 = %q", got)
	}
}

func TestTf(t *testing.T) {
	// используем существующий ключ с плейсхолдером-заглушкой через Tf по messages нет —
	// проверяем механику подстановки на ключе-фолбэке (сам ключ содержит {x}).
	if got := Tf(context.Background(), "greet {x}", "x", "мир"); got != "greet мир" {
		t.Fatalf("Tf = %q", got)
	}
}
