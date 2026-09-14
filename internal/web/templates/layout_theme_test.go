package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/theme"
)

func TestChromelessExplicitThemeSetsDataTheme(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	ctx = theme.WithTheme(ctx, theme.Theme{Code: "dark"})
	var sb strings.Builder
	if err := ErrorPage(404, "", "").Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, `data-theme="dark"`) {
		t.Fatalf("chromeless с явной темой должен печатать data-theme=\"dark\": %s", out)
	}
}

func TestChromelessSystemThemeOmitsDataTheme(t *testing.T) {
	out := renderTo(t, ErrorPage(404, "", ""))
	if strings.Contains(out, "data-theme") {
		t.Fatalf("chromeless с темой system не должен печатать data-theme: %s", out)
	}
}

func TestStatusLayoutExplicitThemeSetsDataTheme(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	ctx = theme.WithTheme(ctx, theme.Theme{Code: "light"})
	v := StatusPageView{Title: "S", Overall: "ok"}
	var sb strings.Builder
	if err := PublicStatusPage(v).Render(ctx, &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, `data-theme="light"`) {
		t.Fatalf("statusLayout с явной темой должен печатать data-theme=\"light\": %s", out)
	}
}

func TestStatusLayoutSystemThemeOmitsDataTheme(t *testing.T) {
	v := StatusPageView{Title: "S", Overall: "ok"}
	out := renderTo(t, PublicStatusPage(v))
	if strings.Contains(out, "data-theme") {
		t.Fatalf("statusLayout с темой system не должен печатать data-theme: %s", out)
	}
}

// statusLayoutBody держит свой <head>, отдельный от layout.templ, — без своей
// theme-color рамка браузера на телефоне остаётся светлой в тёмной теме.
func TestStatusLayoutExplicitThemeHasThemeColor(t *testing.T) {
	v := StatusPageView{Title: "S", Overall: "ok"}
	for _, code := range []string{"dark", "light"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
		ctx = theme.WithTheme(ctx, theme.Theme{Code: code})
		var sb strings.Builder
		if err := PublicStatusPage(v).Render(ctx, &sb); err != nil {
			t.Fatalf("%s: render: %v", code, err)
		}
		out := sb.String()
		want := `<meta name="theme-color" content="` + themeColor(code) + `">`
		if !strings.Contains(out, want) {
			t.Errorf("%s: нет %s в: %s", code, want, out)
		}
	}
}

func TestStatusLayoutSystemThemeHasThemeColor(t *testing.T) {
	v := StatusPageView{Title: "S", Overall: "ok"}
	out := renderTo(t, PublicStatusPage(v))
	for code, media := range map[string]string{"dark": "(prefers-color-scheme: dark)", "light": "(prefers-color-scheme: light)"} {
		want := `<meta name="theme-color" content="` + themeColor(code) + `" media="` + media + `">`
		if !strings.Contains(out, want) {
			t.Errorf("system: нет %s в: %s", want, out)
		}
	}
}
