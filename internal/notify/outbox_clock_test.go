package notify_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

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
