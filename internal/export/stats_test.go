package export

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestStoreQueueSnapshotEmptyQueue(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)

	snap, err := st.QueueSnapshot(context.Background())
	if err != nil {
		t.Fatalf("QueueSnapshot: %v", err)
	}
	if snap.Pending != 0 || snap.Failed != 0 || snap.OldestPendingAge != 0 {
		t.Errorf("пустая очередь: %+v, want нули по всем полям", snap)
	}
}

func TestStoreQueueSnapshotCountsPendingJobs(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	mustEnqueue(t, st, projectID, userID)
	mustEnqueue(t, st, projectID, userID)

	snap, err := st.QueueSnapshot(ctx)
	if err != nil {
		t.Fatalf("QueueSnapshot: %v", err)
	}
	if snap.Pending != 2 {
		t.Errorf("Pending = %d, want 2 — обе заявки в статусе queued не видны", snap.Pending)
	}

	if _, ok, err := st.Claim(ctx); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	snap, err = st.QueueSnapshot(ctx)
	if err != nil {
		t.Fatalf("QueueSnapshot после Claim: %v", err)
	}
	if snap.Pending != 2 {
		t.Errorf("Pending после Claim = %d, want 2 — running-заявка выпала из очереди", snap.Pending)
	}
}

func TestStoreQueueSnapshotCountsFailedJobs(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	id := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status = 'failed', finished_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	snap, err := st.QueueSnapshot(ctx)
	if err != nil {
		t.Fatalf("QueueSnapshot: %v", err)
	}
	if snap.Failed != 1 {
		t.Errorf("Failed = %d, want 1", snap.Failed)
	}
	if snap.Pending != 0 {
		t.Errorf("Pending = %d, want 0 — заявка уже добита, в очереди её нет", snap.Pending)
	}
}

func TestStoreQueueSnapshotOldestPendingAge(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	id := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx,
		`UPDATE export_jobs SET created_at = now() - interval '3 days' WHERE id = $1`, id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustEnqueue(t, st, projectID, userID)

	snap, err := st.QueueSnapshot(ctx)
	if err != nil {
		t.Fatalf("QueueSnapshot: %v", err)
	}
	if snap.OldestPendingAge < 2*24*time.Hour {
		t.Errorf("OldestPendingAge = %s, want >= 48h — трёхсуточная заявка не видна как застрявшая среди более свежих", snap.OldestPendingAge)
	}
}

func TestStatsRunSnapshotsPublishesQueueState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	id := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(context.Background(),
		`UPDATE export_jobs SET status = 'failed', finished_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	mustEnqueue(t, st, projectID, userID)

	stats := &Stats{}
	done := make(chan struct{})
	go func() {
		stats.RunSnapshots(ctx, st)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stats.Pending() == 1 && stats.FailedJobs() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := stats.Pending(); got != 1 {
		t.Errorf("Pending() = %d, want 1", got)
	}
	if got := stats.FailedJobs(); got != 1 {
		t.Errorf("FailedJobs() = %d, want 1", got)
	}
	cancel()
	<-done
}
