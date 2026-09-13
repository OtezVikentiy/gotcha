package telemetry

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Reconcile держит тот же advisory-лок, что и Tick: занятый другой репликой лок
// обязан отменить полный скан ClickHouse, а не только заявки PostgreSQL.
func TestPurgeWorkerReconcileSkipsWhenAnotherInstanceHoldsLock(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const orphan = int64(987_654_323)
	if err := conn.Exec(ctx,
		"INSERT INTO events (event_id, project_id, issue_id, timestamp, user_id, user_ip, user_email) VALUES (generateUUIDv4(), ?, 1, ?, ?, ?, ?)",
		orphan, time.Now().UTC(), "u", "10.0.0.4", "z@y.com"); err != nil {
		t.Fatalf("insert events: %v", err)
	}

	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lockConn.Release()
	var locked bool
	if err := lockConn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(purgeWorkerLockID)).Scan(&locked); err != nil || !locked {
		t.Fatalf("подготовка занятого лока: locked=%v err=%v", locked, err)
	}
	defer lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(purgeWorkerLockID))

	q := NewPurgeQueue(pool)
	w := &PurgeWorker{Queue: q, Purger: NewPurger(conn), Conn: conn}
	n, err := w.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if n != 0 {
		t.Errorf("Reconcile нашёл %d сирот, пока лок держит другая реплика — ожидалось 0", n)
	}

	var queued bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM project_purge_queue WHERE project_id = $1)", orphan).Scan(&queued); err != nil {
		t.Fatalf("чтение очереди: %v", err)
	}
	if queued {
		t.Errorf("сирота поставлен в очередь, пока другая реплика держит лок — сканы ClickHouse не сериализованы")
	}
}
