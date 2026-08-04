package profile

import (
	"context"
	"strings"
	"testing"
	"unicode"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestProfileNotifyLocale — subject/body регрессий профилей строятся из
// каталога i18n по локали в контексте (класс №133–136). До правки тексты
// были зашиты по-английски.
func TestProfileNotifyLocale(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	en := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	const url = "https://gotcha.example/projects/1/profile-regressions"
	ev := ProfileRegressionEvent{
		Service: "api", ProfileType: "cpu", Function: "handler.Serve",
		BaselineShare: 0.2, CurrentShare: 0.35, PctIncrease: 0.75, Opened: true,
	}
	closed := ev
	closed.Opened = false

	if s := regressionSubject(en, ev); !strings.Contains(s, "Profile regression: handler.Serve +75%") {
		t.Errorf("en subject open = %q", s)
	}
	if s := regressionSubject(ru, closed); !strings.Contains(s, "Регрессия профиля устранена: handler.Serve") {
		t.Errorf("ru subject close = %q", s)
	}
	if b := regressionBody(en, ev, url); !strings.Contains(b, "Self-CPU share: 20.0% → 35.0% (+75%)") {
		t.Errorf("en body = %q", b)
	}
	if b := regressionBody(ru, ev, url); !strings.Contains(b, "Доля self-CPU: 20.0% → 35.0% (+75%)") {
		t.Errorf("ru body = %q", b)
	}
	for _, s := range []string{regressionSubject(en, ev), regressionBody(en, ev, url)} {
		if strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
			t.Errorf("кириллица на en-локали: %q", s)
		}
	}
}
