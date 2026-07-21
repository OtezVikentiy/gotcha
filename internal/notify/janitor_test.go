package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestOutboxJanitorRunLifecycle: с крошечным Interval цикл должен хотя бы раз
// тикнуть (вызвав PurgeOld на реальном, пусть и пустом, пуле), а затем
// корректно завершиться по отмене ctx — покрывает обе ветки select
// (ticker.C → работа и ctx.Done → return).
func TestOutboxJanitorRunLifecycle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)

	j := &notify.OutboxJanitor{
		Outbox:    ob,
		Retention: time.Hour,
		Interval:  2 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		j.Run(ctx)
		close(done)
	}()

	// Даём циклу несколько тиков, затем гасим и ждём выхода.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OutboxJanitor.Run did not return after ctx cancel")
	}
}

// TestOutboxJanitorRunDefaultInterval: при Interval<=0 берётся дефолт (1 час),
// то есть тика мы не дождёмся — но ветка выбора дефолта и выход по ctx.Done
// всё равно покрываются. Отменяем ctx сразу, чтобы не ждать час.
func TestOutboxJanitorRunDefaultInterval(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)

	j := &notify.OutboxJanitor{Outbox: ob, Retention: time.Hour, Interval: 0}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		j.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OutboxJanitor.Run did not return after ctx cancel")
	}
}

// TestOutboxMarkErrorsCancelledCtx: MarkRetry/MarkFailed на отменённом ctx —
// пул возвращает ошибку ещё до выполнения SQL, что покрывает ветку
// `if err != nil { return fmt.Errorf(...) }` в обоих методах.
func TestOutboxMarkErrorsCancelledCtx(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := ob.MarkRetry(ctx, 1, errors.New("smtp timeout"), time.Now()); err == nil {
		t.Error("MarkRetry on cancelled ctx: got nil error, want DB error")
	}
	if err := ob.MarkFailed(ctx, 1, errors.New("giving up")); err == nil {
		t.Error("MarkFailed on cancelled ctx: got nil error, want DB error")
	}
}
