package templates

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/theme"
)

// TestChromelessExplicitThemeSetsDataTheme — chromeless (обвязка страниц
// логина/ошибок/onboarding) ветвится по theme.FromContext(ctx).Code: при
// "system" тег <html> идёт без data-theme (тема выбирается CSS через
// prefers-color-scheme), при явном выборе — с атрибутом data-theme.
// Существующие тесты ErrorPage рендерят только через renderTo (дефолтный
// контекст, тема "system"), поэтому ветка с явной темой не исполнялась ни
// разу.
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

// TestChromelessSystemThemeOmitsDataTheme — обратная ветка: тема "system"
// (дефолт контекста) не должна печатать атрибут data-theme вовсе.
func TestChromelessSystemThemeOmitsDataTheme(t *testing.T) {
	out := renderTo(t, ErrorPage(404, "", ""))
	if strings.Contains(out, "data-theme") {
		t.Fatalf("chromeless с темой system не должен печатать data-theme: %s", out)
	}
}

// TestStatusLayoutExplicitThemeSetsDataTheme — та же развилка theme.Code
// у statusLayout (независимый layout публичной статус-страницы, не делит
// код с chromeless/layout). Существующие тесты PublicStatusPage тоже все
// шли через renderTo с темой "system".
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

// TestStatusLayoutSystemThemeOmitsDataTheme — обратная ветка statusLayout.
func TestStatusLayoutSystemThemeOmitsDataTheme(t *testing.T) {
	v := StatusPageView{Title: "S", Overall: "ok"}
	out := renderTo(t, PublicStatusPage(v))
	if strings.Contains(out, "data-theme") {
		t.Fatalf("statusLayout с темой system не должен печатать data-theme: %s", out)
	}
}
