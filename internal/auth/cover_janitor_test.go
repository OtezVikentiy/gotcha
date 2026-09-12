package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestJanitorRunDefaultsIntervalWhenZero(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)

	j := &Janitor{Svc: svc} // Interval нулевой — дефолт должен подставиться сам
	ctx, cancel := context.WithCancel(context.Background())
	// time.NewTicker (и потенциальная паника) выполняется до первого обращения
	// к ctx — порядок отмены относительно старта горутины не важен.
	cancel()

	var panicVal any
	done := make(chan struct{})
	go func() {
		defer func() {
			panicVal = recover()
			close(done)
		}()
		j.Run(ctx)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не вернулся после отмены ctx")
	}
	if panicVal != nil {
		t.Fatalf("Run с нулевым Interval запаниковал: %v (подстановка interval<=0 сломана)", panicVal)
	}
}

func TestJanitorTickHandlesExtraCleanupsAndDBError(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)

	dead, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var failCalled, okCalled bool
	j := &Janitor{
		Svc: svc,
		Extra: []Cleanup{
			{Name: "failing", Fn: func(context.Context) (int64, error) {
				failCalled = true
				return 0, errors.New("boom")
			}},
			{Name: "ok", Fn: func(context.Context) (int64, error) {
				okCalled = true
				return 3, nil
			}},
		},
	}

	j.tick(dead)

	if !failCalled {
		t.Fatal("Extra[0].Fn (падающая очистка) не вызван")
	}
	if !okCalled {
		t.Fatal("Extra[1].Fn (успешная очистка) не вызван — после ошибки первой очистки tick не должен останавливаться")
	}

	log := buf.String()
	if !strings.Contains(log, "delete expired sessions failed") {
		t.Fatalf("лог не содержит сообщение об ошибке DeleteExpiredSessions: %s", log)
	}
	if !strings.Contains(log, "cleanup failed") || !strings.Contains(log, "cleanup=failing") {
		t.Fatalf("лог не содержит сообщение об ошибке Extra-очистки failing: %s", log)
	}
	var okLine string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "auth janitor: cleanup done") {
			okLine = line
		}
	}
	if !strings.Contains(okLine, "cleanup=ok") {
		t.Fatalf("лог не содержит сообщение об успешной Extra-очистке ok: %s", log)
	}
	// Число из Fn (3), не произвольное — иначе лог мог бы нести любой count.
	if !strings.Contains(okLine, "count=3") {
		t.Fatalf("лог успешной Extra-очистки несёт не то count (want count=3): %s", okLine)
	}
}
