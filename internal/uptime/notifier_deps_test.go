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

// stubDepCounter — фиксированный ответ depsLine, запоминающий аргументы
// вызова: без этого подмена kind/nodeID прошла бы мимо тестов.
type stubDepCounter struct {
	cnt     int
	err     error
	gotKind string
	gotNode int64
}

func (s *stubDepCounter) DeclaredChildrenCount(_ context.Context, kind string, nodeID int64) (int, error) {
	s.gotKind, s.gotNode = kind, nodeID
	return s.cnt, s.err
}

// TestUptimeNotifierDepsLineGate — гейты depsLine монитора: строка есть
// только у down-события с ненулевым счётчиком; nil-счётчик, иной вид
// события, ноль детей и ошибка счётчика дают пустую строку (fail-open, Р9).
// Отдельно пинуются аргументы счётчика — («monitor», ID монитора).
func TestUptimeNotifierDepsLineGate(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	down := Event{Kind: "down", Monitor: Monitor{ID: 7, Name: "api-prod"}}

	counter := &stubDepCounter{cnt: 2}
	n := &OutboxNotifier{DepCounts: counter}
	if got := n.depsLine(ctx, down); got != "\nЗависимых узлов: 2" {
		t.Fatalf("down with 2 children: got %q", got)
	}
	if counter.gotKind != "monitor" || counter.gotNode != down.Monitor.ID {
		t.Fatalf("counter must be asked about (monitor, %d), got (%q, %d)",
			down.Monitor.ID, counter.gotKind, counter.gotNode)
	}

	up := down
	up.Kind = "up"
	if got := n.depsLine(ctx, up); got != "" {
		t.Fatalf("non-down kind must not get deps line, got %q", got)
	}
	if got := (&OutboxNotifier{}).depsLine(ctx, down); got != "" {
		t.Fatalf("nil DepCounts must be silent, got %q", got)
	}
	if got := (&OutboxNotifier{DepCounts: &stubDepCounter{cnt: 0}}).depsLine(ctx, down); got != "" {
		t.Fatalf("zero children must be silent, got %q", got)
	}
	if got := (&OutboxNotifier{DepCounts: &stubDepCounter{err: context.DeadlineExceeded}}).depsLine(ctx, down); got != "" {
		t.Fatalf("counter error must fail open with empty line, got %q", got)
	}
}
