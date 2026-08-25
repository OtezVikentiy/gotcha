package host

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestHostBodyDepsLine — строка «Зависимых узлов: N» в open-теле инцидента
// недоступности (D3 Р9): непустой depsLine попадает в реальный рендер
// каталога, пустой не оставляет ни текста, ни висящего плейсхолдера
// {deps_line}, ни лишней пустой строки.
func TestHostBodyDepsLine(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	in := Incident{Kind: "silent", CurrentValue: 120}
	h := Host{Name: "gw-1"}

	body := hostBody(ctx, in, h, true, 0, false, "https://x/link", "\nЗависимых узлов: 3")
	if !strings.Contains(body, "Зависимых узлов: 3") {
		t.Fatalf("open body must contain deps line, got %q", body)
	}

	body = hostBody(ctx, in, h, true, 0, false, "https://x/link", "")
	if strings.Contains(body, "Зависимых узлов") || strings.Contains(body, "{deps_line}") {
		t.Fatalf("empty deps line must leave no trace, got %q", body)
	}
	if strings.Contains(body, "\n\nhttps://x/link") {
		t.Fatalf("empty deps line must not leave a blank line before url, got %q", body)
	}
}

// stubDepCounter — фиксированный ответ для depsLine.
type stubDepCounter struct {
	cnt int
	err error
}

func (s stubDepCounter) DeclaredChildrenCount(context.Context, string, int64) (int, error) {
	return s.cnt, s.err
}

// TestHostNotifierDepsLineGate — гейты depsLine: строка есть только у
// silent-инцидента с ненулевым счётчиком; nil-счётчик, не-silent вид,
// ноль детей и ошибка счётчика дают пустую строку (fail-open, Р9).
func TestHostNotifierDepsLineGate(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	h := Host{ID: 7, Name: "gw-1"}
	silent := Incident{Kind: "silent"}

	n := &HostNotifier{DepCounts: stubDepCounter{cnt: 3}}
	if got := n.depsLine(ctx, silent, h); got != "\nЗависимых узлов: 3" {
		t.Fatalf("silent with 3 children: got %q", got)
	}
	if got := n.depsLine(ctx, Incident{Kind: "disk"}, h); got != "" {
		t.Fatalf("non-silent kind must not get deps line, got %q", got)
	}
	if got := (&HostNotifier{}).depsLine(ctx, silent, h); got != "" {
		t.Fatalf("nil DepCounts must be silent, got %q", got)
	}
	if got := (&HostNotifier{DepCounts: stubDepCounter{cnt: 0}}).depsLine(ctx, silent, h); got != "" {
		t.Fatalf("zero children must be silent, got %q", got)
	}
	if got := (&HostNotifier{DepCounts: stubDepCounter{err: context.DeadlineExceeded}}).depsLine(ctx, silent, h); got != "" {
		t.Fatalf("counter error must fail open with empty line, got %q", got)
	}
}
