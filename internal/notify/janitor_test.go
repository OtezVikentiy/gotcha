package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestOutboxJanitorRunLifecycle: цикл должен РЕАЛЬНО чистить очередь и
// корректно завершаться по отмене ctx.
//
// Раньше единственным утверждением было «горутина вышла после cancel», и тест
// оставался зелёным, даже если вырезать тело ветки ticker.C целиком: он
// проверял, что цикл завершается, а не что он работает. Теперь в очередь
// кладётся протухшая строка, и тест ждёт её исчезновения — то есть факта
// выполненной работы, а не факта выхода.
func TestOutboxJanitorRunLifecycle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	ctx0 := context.Background()

	channelID := newChannel(t, pool)
	if err := ob.Enqueue(ctx0, channelID, map[string]any{"kind": "test"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Строка чистится, только если она уже доставлена (или провалена) и старше
	// срока хранения — состариваем её прямо в базе.
	if _, err := pool.Exec(ctx0,
		"UPDATE notification_outbox SET status='sent', created_at = now() - interval '2 hours'"); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	j := &notify.OutboxJanitor{
		Outbox:    ob,
		Retention: time.Hour,
		Interval:  2 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(ctx0)
	done := make(chan struct{})
	go func() {
		j.Run(ctx)
		close(done)
	}()

	// Ждём именно результата работы, а не истечения сна.
	deadline := time.After(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx0, "SELECT count(*) FROM notification_outbox").Scan(&n); err != nil {
			t.Fatalf("count outbox: %v", err)
		}
		if n == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("протухшая строка не вычищена — цикл не делает работу")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OutboxJanitor.Run did not return after ctx cancel")
	}
}

// TestOutboxJanitorRunDefaultInterval: при Interval<=0 берётся ОСМЫСЛЕННЫЙ
// дефолт.
//
// Раньше тест отменял контекст сразу после запуска и проверял только выход
// горутины: дефолт можно было поменять с часа на наносекунду — постоянный
// обстрел собственной базы — и тест бы этого не заметил. Теперь он требует,
// чтобы за заметное время чистка НЕ случилась.
func TestOutboxJanitorRunDefaultInterval(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	ctx0 := context.Background()

	channelID := newChannel(t, pool)
	if err := ob.Enqueue(ctx0, channelID, map[string]any{"kind": "test"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := pool.Exec(ctx0,
		"UPDATE notification_outbox SET status='sent', created_at = now() - interval '2 hours'"); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	j := &notify.OutboxJanitor{Outbox: ob, Retention: time.Hour, Interval: 0}

	ctx, cancel := context.WithCancel(ctx0)
	done := make(chan struct{})
	go func() {
		j.Run(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	var n int
	if err := pool.QueryRow(ctx0, "SELECT count(*) FROM notification_outbox").Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n == 0 {
		cancel()
		t.Fatal("при Interval=0 чистка сработала за 50 мс — дефолтный интервал слишком мал")
	}

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

	if err := ob.MarkRetry(ctx, 1, errors.New("smtp timeout"), time.Minute); err == nil {
		t.Error("MarkRetry on cancelled ctx: got nil error, want DB error")
	}
	if err := ob.MarkFailed(ctx, 1, errors.New("giving up")); err == nil {
		t.Error("MarkFailed on cancelled ctx: got nil error, want DB error")
	}
}
