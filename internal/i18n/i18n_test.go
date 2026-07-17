package i18n

import (
	"context"
	"testing"
)

func TestTLookupAndFallback(t *testing.T) {
	ru := WithLocale(context.Background(), Locale{Code: "ru"})
	if got := T(ru, "nav.projects"); got != "Проекты" {
		t.Fatalf("ru nav.projects = %q", got)
	}
	en := WithLocale(context.Background(), Locale{Code: "en"})
	if got := T(en, "nav.projects"); got != "Projects" {
		t.Fatalf("en nav.projects = %q", got)
	}
	// нет локали в ctx → Default (ru)
	if got := T(context.Background(), "action.logout"); got != "Выйти" {
		t.Fatalf("default action.logout = %q", got)
	}
	// отсутствующий ключ → сам ключ (видимый маркер)
	if got := T(en, "no.such.key"); got != "no.such.key" {
		t.Fatalf("missing key = %q", got)
	}
}
