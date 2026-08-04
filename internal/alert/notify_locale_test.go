package alert

import (
	"context"
	"strings"
	"testing"
	"unicode"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestIssueAlertKindLabelLocale — вид алерта в теме письма локализуется
// (класс №133–136): раньше в тему уходил сырой enum ("new_issue").
// Незнакомый вид уходит как есть, а не прячется за пустой строкой.
func TestIssueAlertKindLabelLocale(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	en := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})

	for kind, want := range map[string]string{
		"new_issue": "New issue", "regression": "Regression", "spike": "Spike",
	} {
		got := issueAlertKindLabel(en, kind)
		if got != want {
			t.Errorf("en %s = %q, want %q", kind, got, want)
		}
		if strings.ContainsFunc(got, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
			t.Errorf("кириллица на en-локали: %q", got)
		}
	}
	if got := issueAlertKindLabel(ru, "new_issue"); got != "Новая проблема" {
		t.Errorf("ru new_issue = %q", got)
	}
	if got := issueAlertKindLabel(en, "mystery"); got != "mystery" {
		t.Errorf("unknown kind = %q, want passthrough", got)
	}
}
