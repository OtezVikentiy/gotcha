package telemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestPurgeQueueClaimIsExclusive — заявку забирает ровно один claim: после
// снятия заявки очередь пуста. FOR UPDATE SKIP LOCKED виден только внутри
// чужой транзакции, поэтому проверяем то, что действительно гарантировано:
// заявка отдаётся один раз, а после Done не отдаётся вовсе.
func TestPurgeQueueClaimIsExclusive(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	q := telemetry.NewPurgeQueue(pool)
	if err := q.Enqueue(ctx, pid); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, ok, err := q.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim: got=%d ok=%v err=%v", got, ok, err)
	}
	if got != pid {
		t.Fatalf("Claim вернул проект %d, ожидался %d", got, pid)
	}
	if err := q.Done(ctx, pid); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if _, ok, err := q.Claim(ctx); err != nil || ok {
		t.Fatalf("после Done очередь обязана быть пуста: ok=%v err=%v", ok, err)
	}
}

// TestPurgeQueueEnqueueIsIdempotent — повторная заявка на тот же проект не
// заводит вторую строку: очистка проекта нужна один раз, сколько бы раз о ней
// ни попросили (штатное удаление плюс сверка сирот).
func TestPurgeQueueEnqueueIsIdempotent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	q := telemetry.NewPurgeQueue(pool)
	if err := q.Enqueue(ctx, pid); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.Enqueue(ctx, pid, pid); err != nil {
		t.Fatalf("Enqueue повторно: %v", err)
	}
	depth, _, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if depth != 1 {
		t.Errorf("глубина очереди %d после трёх заявок на один проект, ожидалась 1", depth)
	}
}

// TestPurgeQueueFailKeepsRequest — невыполненное удаление персональных данных
// не списывается в потери: заявка остаётся, попытка засчитана, причина
// записана.
func TestPurgeQueueFailKeepsRequest(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	q := telemetry.NewPurgeQueue(pool)
	if err := q.Enqueue(ctx, pid); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, _, err := q.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := q.Fail(ctx, pid, errors.New("clickhouse недоступен")); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	var attempts int
	var lastErr string
	if err := pool.QueryRow(ctx,
		"SELECT attempts, coalesce(last_error, '') FROM project_purge_queue WHERE project_id = $1",
		pid).Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, ожидалась 1", attempts)
	}
	if !strings.Contains(lastErr, "clickhouse недоступен") {
		t.Errorf("last_error = %q, причина отказа не записана", lastErr)
	}
	if _, ok, err := q.Claim(ctx); err != nil || !ok {
		t.Fatalf("заявка после отказа обязана оставаться в очереди: ok=%v err=%v", ok, err)
	}
}

// TestPurgeQueueFailTruncatesLongCause — ошибка драйвера может нести весь
// текст запроса; заявка не журнал, и обрезка не должна ломать кириллицу
// пополам.
func TestPurgeQueueFailTruncatesLongCause(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	q := telemetry.NewPurgeQueue(pool)
	if err := q.Enqueue(ctx, pid); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	long := strings.Repeat("ошибка ", 500)
	if err := q.Fail(ctx, pid, errors.New(long)); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	var stored string
	if err := pool.QueryRow(ctx,
		"SELECT coalesce(last_error, '') FROM project_purge_queue WHERE project_id = $1",
		pid).Scan(&stored); err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if n := len([]rune(stored)); n > 500 {
		t.Errorf("в заявке %d символов причины, ожидалось не больше 500", n)
	}
	if !strings.HasPrefix(stored, "ошибка ") {
		t.Errorf("причина обрезана по границе байта, а не руны: %q…", stored[:20])
	}
}

// TestPurgeQueueStats — наблюдаемость: оператор, обязанный удалить данные,
// должен видеть, что обязанность не исполнена.
func TestPurgeQueueStats(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	q := telemetry.NewPurgeQueue(pool)
	if depth, oldest, err := q.Stats(ctx); err != nil || depth != 0 || oldest != 0 {
		t.Fatalf("пустая очередь: depth=%d oldest=%d err=%v, ожидались нули", depth, oldest, err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO project_purge_queue (project_id, enqueued_at) VALUES ($1, now() - interval '3 days')",
		pid); err != nil {
		t.Fatalf("seed: %v", err)
	}
	depth, oldest, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if depth < 1 {
		t.Errorf("depth = %d, заявка в очереди не видна", depth)
	}
	if oldest < 2*24*3600 {
		t.Errorf("oldest = %d секунд, заявка трёхсуточной давности не видна как застрявшая", oldest)
	}
}

// TestPurgeWorkerRemovesTelemetryAndRequest — заявка снимается только после
// того, как данные действительно удалены.
func TestPurgeWorkerRemovesTelemetryAndRequest(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	ts := time.Now().UTC()
	seedEvents(t, ctx, conn, pid, "u1", "10.0.0.1", "a@b.com", ts)
	seedTransactions(t, ctx, conn, pid, "u1", ts)

	q := telemetry.NewPurgeQueue(pool)
	if err := q.Enqueue(ctx, pid); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	w := &telemetry.PurgeWorker{Queue: q, Purger: telemetry.NewPurger(conn)}
	n, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("Tick обработал %d заявок, ожидалась 1", n)
	}

	for _, table := range []string{"events", "transactions"} {
		if got := count(t, ctx, conn, table, pid); got != 0 {
			t.Errorf("%s: осталось %d строк удалённого проекта", table, got)
		}
	}
	depth, _, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if depth != 0 {
		t.Errorf("заявка не снята: глубина очереди %d", depth)
	}
	if w.Purged() != 1 {
		t.Errorf("Purged() = %d, ожидалась 1", w.Purged())
	}
}

// TestPurgeWorkerDrainsQueue — проход разгребает очередь целиком, а не одну
// заявку за тик: иначе удаление организации с двадцатью проектами
// растягивалось бы на двадцать периодов.
func TestPurgeWorkerDrainsQueue(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	ids := []int64{newEntityProject(t, pool), newEntityProject(t, pool), newEntityProject(t, pool)}
	q := telemetry.NewPurgeQueue(pool)
	if err := q.Enqueue(ctx, ids...); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	w := &telemetry.PurgeWorker{Queue: q, Purger: telemetry.NewPurger(conn)}
	n, err := w.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != len(ids) {
		t.Errorf("Tick обработал %d заявок из %d — очередь разгребается по одной за период", n, len(ids))
	}
}

// TestPurgeWorkerKeepsRequestOnFailure — отказ мутации оставляет заявку,
// записав причину: невыполненное удаление персональных данных не списывается
// в потери. Ошибка настоящая — соединение с ClickHouse закрыто (у каждого
// теста оно своё, см. testenv.MigratedCH), а не подставлена моком.
func TestPurgeWorkerKeepsRequestOnFailure(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	q := telemetry.NewPurgeQueue(pool)
	if err := q.Enqueue(ctx, pid); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	p := telemetry.NewPurger(conn)
	if err := conn.Close(); err != nil {
		t.Fatalf("close ch: %v", err)
	}
	w := &telemetry.PurgeWorker{Queue: q, Purger: p}
	if _, err := w.Tick(ctx); err == nil {
		t.Fatalf("Tick при недоступном ClickHouse обязан вернуть ошибку")
	}

	var attempts int
	var lastErr string
	if err := pool.QueryRow(ctx,
		"SELECT attempts, coalesce(last_error, '') FROM project_purge_queue WHERE project_id = $1",
		pid).Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("заявка потеряна после неудачной попытки: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, ожидалась 1", attempts)
	}
	if lastErr == "" {
		t.Errorf("причина отказа не записана — оператор не узнает, почему данные ещё живы")
	}
	if w.Purged() != 0 {
		t.Errorf("Purged() = %d при неудачной попытке", w.Purged())
	}
}

// TestPurgeWorkerNilPurgerIsNoop — стенд без ClickHouse не должен ронять
// проход: заявки просто ждут.
func TestPurgeWorkerNilPurgerIsNoop(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newEntityProject(t, pool)

	q := telemetry.NewPurgeQueue(pool)
	if err := q.Enqueue(ctx, pid); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	w := &telemetry.PurgeWorker{Queue: q}
	n, err := w.Tick(ctx)
	if err != nil || n != 0 {
		t.Fatalf("Tick без Purger: n=%d err=%v, ожидались 0 и nil", n, err)
	}
	depth, _, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if depth != 1 {
		t.Errorf("глубина очереди %d, заявка должна была остаться", depth)
	}
}

// TestPurgeWorkerReconcileEnqueuesOrphans — сверка ставит заявку на проект,
// которого в PostgreSQL уже нет, и не трогает живой. Ошибка в другую сторону
// стоила бы данных работающего проекта, поэтому проверяются оба исхода.
func TestPurgeWorkerReconcileEnqueuesOrphans(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	live := newEntityProject(t, pool)
	const orphan = int64(987_654_321) // такого проекта в PostgreSQL нет
	ts := time.Now().UTC()
	seedEvents(t, ctx, conn, live, "u1", "10.0.0.1", "a@b.com", ts)
	seedEvents(t, ctx, conn, orphan, "u2", "10.0.0.2", "c@d.com", ts)

	q := telemetry.NewPurgeQueue(pool)
	w := &telemetry.PurgeWorker{Queue: q, Purger: telemetry.NewPurger(conn), Conn: conn}
	n, err := w.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if n != 1 {
		t.Errorf("Reconcile нашёл %d сирот, ожидалась 1", n)
	}

	var queuedOrphan, queuedLive bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM project_purge_queue WHERE project_id = $1)", orphan).Scan(&queuedOrphan); err != nil {
		t.Fatalf("чтение очереди: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM project_purge_queue WHERE project_id = $1)", live).Scan(&queuedLive); err != nil {
		t.Fatalf("чтение очереди: %v", err)
	}
	if !queuedOrphan {
		t.Errorf("сирота %d не поставлен в очередь — телеметрия удалённого проекта останется навсегда", orphan)
	}
	if queuedLive {
		t.Errorf("живой проект %d поставлен на удаление — сверка удаляет данные работающего проекта", live)
	}
}

// TestPurgeWorkerReconcileDeletesNothing — сверка только ставит заявки.
// Удаление идёт единственным путём, который уже проверен, поэтому ошибка
// сверки обязана давать лишнюю заявку, а не потерянные данные.
func TestPurgeWorkerReconcileDeletesNothing(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const orphan = int64(987_654_322)
	seedEvents(t, ctx, conn, orphan, "u", "10.0.0.3", "x@y.com", time.Now().UTC())

	q := telemetry.NewPurgeQueue(pool)
	w := &telemetry.PurgeWorker{Queue: q, Purger: telemetry.NewPurger(conn), Conn: conn}
	if _, err := w.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := count(t, ctx, conn, "events", orphan); got == 0 {
		t.Errorf("сверка удалила данные сама — удаление обязано идти только через очередь")
	}
}

// TestPurgeWorkerReconcileWithoutConnIsNoop — стенд без ClickHouse: сверка
// молча пропускается, а не роняет проход.
func TestPurgeWorkerReconcileWithoutConnIsNoop(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	w := &telemetry.PurgeWorker{Queue: telemetry.NewPurgeQueue(pool)}
	n, err := w.Reconcile(ctx)
	if err != nil || n != 0 {
		t.Fatalf("Reconcile без Conn: n=%d err=%v, ожидались 0 и nil", n, err)
	}
}
