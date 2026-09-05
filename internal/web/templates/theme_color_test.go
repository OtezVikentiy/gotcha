package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/theme"
)

// TestHeadThemeColorExplicitTheme — при явной теме <head> печатает одну
// <meta name="theme-color"> с полотном этой темы и без media: тема выбрана
// пользователем, а не системой, и рамка браузера не должна ходить за
// prefers-color-scheme.
func TestHeadThemeColorExplicitTheme(t *testing.T) {
	for _, code := range []string{"dark", "light"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
		ctx = theme.WithTheme(ctx, theme.Theme{Code: code})
		var sb strings.Builder
		if err := ErrorPage(404, "", "").Render(ctx, &sb); err != nil {
			t.Fatalf("%s: render: %v", code, err)
		}
		out := sb.String()
		want := `<meta name="theme-color" content="` + themeColor(code) + `">`
		if strings.Count(out, `name="theme-color"`) != 1 {
			t.Fatalf("%s: ожидалась ровно одна <meta name=\"theme-color\">: %s", code, out)
		}
		if !strings.Contains(out, want) {
			t.Errorf("%s: нет %s в: %s", code, want, out)
		}
		if strings.Contains(out, `prefers-color-scheme`) {
			t.Errorf("%s: явная тема не должна печатать media-варианты theme-color", code)
		}
	}
}

// TestHeadThemeColorSystemTheme — тема «system»: две <meta> с media по
// prefers-color-scheme, по одной на каждое полотно — ровно так же app.css
// выбирает палитру.
func TestHeadThemeColorSystemTheme(t *testing.T) {
	out := renderTo(t, ErrorPage(404, "", ""))
	for code, media := range map[string]string{"dark": "(prefers-color-scheme: dark)", "light": "(prefers-color-scheme: light)"} {
		want := `<meta name="theme-color" content="` + themeColor(code) + `" media="` + media + `">`
		if !strings.Contains(out, want) {
			t.Errorf("system: нет %s в: %s", want, out)
		}
	}
	if got := strings.Count(out, `name="theme-color"`); got != 2 {
		t.Errorf("system: ожидались две <meta name=\"theme-color\">, получено %d", got)
	}
}
