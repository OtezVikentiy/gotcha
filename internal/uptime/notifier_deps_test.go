package uptime

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestBodyForDepsLine — строка «Зависимых узлов: N» в down-теле уведомления
// монитора (D3 Р9): непустой depsLine попадает в реальный рендер каталога,
// пустой не оставляет ни текста, ни висящего плейсхолдера {deps_line}.
// Остальные виды событий плейсхолдера в шаблонах не имеют вовсе.
func TestBodyForDepsLine(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	const url = "https://gotcha.example/monitors/7"
	down := Event{
		Kind:    "down",
		Monitor: Monitor{ID: 7, Name: "api-prod"},
		Regions: []string{"local"},
		Cause:   "timeout",
	}

	body := bodyFor(ctx, down, url, "\nЗависимых узлов: 2")
	if !strings.Contains(body, "Зависимых узлов: 2") {
		t.Fatalf("down body must contain deps line, got %q", body)
	}

	body = bodyFor(ctx, down, url, "")
	if strings.Contains(body, "Зависимых узлов") || strings.Contains(body, "{deps_line}") {
		t.Fatalf("empty deps line must leave no trace, got %q", body)
	}
	if !strings.Contains(body, "Регионы: local\n\n"+url) {
		t.Fatalf("empty deps line must not leave a blank line after regions, got %q", body)
	}

	up := down
	up.Kind, up.DurationSeconds = "up", 125
	if body := bodyFor(ctx, up, url, "\nЗависимых узлов: 2"); strings.Contains(body, "Зависимых узлов") {
		t.Fatalf("non-down body must ignore deps line, got %q", body)
	}
}
