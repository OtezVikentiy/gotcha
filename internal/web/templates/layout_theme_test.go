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
