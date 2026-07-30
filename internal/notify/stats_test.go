package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestQueueSnapshotSeesStuckQueue: возраст самой старой ждущей задачи —
// единственное число, отличающее «очередь пуста, потому что всё доставлено» от
// «очередь стоит». До него «алерт не пришёл» диагностировался грепом логов.
func TestQueueSnapshotSeesStuckQueue(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ob := notify.NewOutbox(pool)
	channelID := newChannel(t, pool)

	empty, err := ob.QueueSnapshot(ctx)
	if err != nil {
		t.Fatalf("QueueSnapshot: %v", err)
	}
	if empty.Pending != 0 || empty.OldestPendingAge != 0 {
		t.Fatalf("пустая очередь = %+v, want нули", empty)
	}

	if err := ob.Enqueue(ctx, channelID, map[string]any{"channel_kind": "email"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Состарим задачу: снимок должен показать возраст, а не «только что».
	if _, err := pool.Exec(ctx,
		"UPDATE notification_outbox SET created_at = now() - interval '2 hours'"); err != nil {
		t.Fatalf("состарить задачу: %v", err)
	}

	snap, err := ob.QueueSnapshot(ctx)
	if err != nil {
		t.Fatalf("QueueSnapshot: %v", err)
	}
	if snap.Pending != 1 {
		t.Errorf("Pending = %d, want 1", snap.Pending)
	}
	if snap.OldestPendingAge < 90*time.Minute {
		t.Errorf("OldestPendingAge = %v, want ~2ч: стоящая очередь неотличима от пустой", snap.OldestPendingAge)
	}
}

// TestQueueSnapshotCountsFailedSeparately: задачи, у которых кончились попытки,
// не должны считаться ждущими — иначе очередь выглядит вечно занятой.
func TestQueueSnapshotCountsFailedSeparately(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ob := notify.NewOutbox(pool)
	channelID := newChannel(t, pool)

	if err := ob.Enqueue(ctx, channelID, map[string]any{"channel_kind": "email"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: %+v err=%v", jobs, err)
	}
	if err := ob.MarkFailed(ctx, jobs[0].ID, errors.New("smtp refused")); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	snap, err := ob.QueueSnapshot(ctx)
	if err != nil {
		t.Fatalf("QueueSnapshot: %v", err)
	}
	if snap.Failed != 1 {
		t.Errorf("Failed = %d, want 1", snap.Failed)
	}
	if snap.Pending != 0 {
		t.Errorf("Pending = %d, want 0: провалившаяся задача не ждёт доставки", snap.Pending)
	}
	if snap.OldestPendingAge != 0 {
		t.Errorf("OldestPendingAge = %v, want 0", snap.OldestPendingAge)
	}
}
