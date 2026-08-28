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

// TestJanitorRunDefaultsIntervalWhenZero — Run обязан подставить
// defaultJanitorInterval, если Janitor.Interval не задан (<=0): без этой
// подстановки time.NewTicker получает неположительный duration и паникует.
// Горутина ловит панику явно через recover — если защиту вырезать, тест
// падает на конкретном t.Fatalf, а не крашем процесса.
func TestJanitorRunDefaultsIntervalWhenZero(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)

	j := &Janitor{Svc: svc} // Interval нулевой — дефолт должен подставиться сам
	ctx, cancel := context.WithCancel(context.Background())
	// Отменяем сразу: time.NewTicker(interval) — и потенциальная паника на
	// нём — выполняется до первого обращения к ctx, так что порядок отмены
	// относительно старта горутины не влияет на срабатывание проверяемой
	// защиты. Так тест не зависит от фиксированной паузы и не флапает на
	// медленном раннере.
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

// TestJanitorTickHandlesExtraCleanupsAndDBError — tick() должен: (1) залогировать
// и проглотить ошибку DeleteExpiredSessions, не прерывая обработку Extra;
// (2) на ошибке одной Extra-очистки залогировать её и перейти к следующей
// через continue; (3) успешную Extra-очистку тоже отработать и залогировать
// debug-сообщением. Отменённый ctx — тот же приём, что и в errbranch_test.go:
// детерминированно роняет DeleteExpiredSessions без гонок и без моков поверх
// pgx. Проверяем не только сам факт вызова Extra.Fn (это доказывает лишь, что
// цикл не остановился), но и содержимое лога для каждой из трёх веток —
// иначе мутация, стирающая конкретный slog-вызов при сохранении continue,
// прошла бы тест незамеченной.
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
	// Extra[1].Fn возвращает m=3 — лог обязан нести именно это число, а не
	// произвольное (иначе можно залогировать любой count и тест не заметит).
	if !strings.Contains(okLine, "count=3") {
		t.Fatalf("лог успешной Extra-очистки несёт не то count (want count=3): %s", okLine)
	}
}
