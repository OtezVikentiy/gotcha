package host

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

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

func TestHostNotifierDepsLineGate(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	h := Host{ID: 7, Name: "gw-1"}
	silent := Incident{Kind: "silent"}

	counter := &stubDepCounter{cnt: 3}
	n := &HostNotifier{DepCounts: counter}
	if got := n.depsLine(ctx, silent, h); got != "\nЗависимых узлов: 3" {
		t.Fatalf("silent with 3 children: got %q", got)
	}
	if counter.gotKind != "host" || counter.gotNode != h.ID {
		t.Fatalf("counter must be asked about (host, %d), got (%q, %d)",
			h.ID, counter.gotKind, counter.gotNode)
	}
	if got := n.depsLine(ctx, Incident{Kind: "disk"}, h); got != "" {
		t.Fatalf("non-silent kind must not get deps line, got %q", got)
	}
	if got := (&HostNotifier{}).depsLine(ctx, silent, h); got != "" {
		t.Fatalf("nil DepCounts must be silent, got %q", got)
	}
	if got := (&HostNotifier{DepCounts: &stubDepCounter{cnt: 0}}).depsLine(ctx, silent, h); got != "" {
		t.Fatalf("zero children must be silent, got %q", got)
	}
	if got := (&HostNotifier{DepCounts: &stubDepCounter{err: context.DeadlineExceeded}}).depsLine(ctx, silent, h); got != "" {
		t.Fatalf("counter error must fail open with empty line, got %q", got)
	}
}
