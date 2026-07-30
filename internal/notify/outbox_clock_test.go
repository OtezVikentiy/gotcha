package notify_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestClaimLeaseUsesDatabaseClock — лиза задачи отсчитывается часами БАЗЫ.
//
// Раньше next_retry_at писался как time.Now().Add(claimLease) часами процесса,
// а видимость задачи проверялась через now() базы в том же запросе. Отставание
// часов контейнера больше лизы выдавало лизу уже просроченной: задача
// оставалась видимой и уходила повторно каждым воркером на каждом тике — дубли
// сообщений у получателя, потому что провайдерской идемпотентности ни у
// Telegram, ни у вебхуков нет.
//
// Тест не подменяет часы: инжекция вернула бы ровно ту лазейку, из которой
// дефект и растёт. Он закрепляет свойство «после Claim задача не видна», а
// доказательство того, что свойство держится именно вычислением в SQL, даёт
// проверка next_retry_at против now() базы.
func TestClaimLeaseUsesDatabaseClock(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	ch := newChannel(t, pool)

	if err := ob.Enqueue(ctx, ch, map[string]any{"kind": "test"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	first, err := ob.Claim(ctx, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("Claim = %+v err=%v, want one job", first, err)
	}

	again, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim повторно: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("повторный Claim вернул %d задач — лиза не действует", len(again))
	}

	var aheadOfDBClock bool
	if err := pool.QueryRow(ctx,
		"SELECT next_retry_at > now() FROM notification_outbox LIMIT 1").Scan(&aheadOfDBClock); err != nil {
		t.Fatalf("read next_retry_at: %v", err)
	}
	if !aheadOfDBClock {
		t.Fatal("next_retry_at не в будущем по часам базы — лиза посчитана часами процесса")
	}
}
