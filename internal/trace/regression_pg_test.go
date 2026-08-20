package trace_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestRegressionOpenConcurrentOnlyOneWins: инвариант «один открытый инцидент на
// цель» под гонкой. N горутин одновременно зовут Open по одной цели — ровно одна
// должна получить created=true и в таблице должна остаться ровно одна строка
// (частичный уникальный индекс — арбитр ON CONFLICT). Именно на этом инварианте
// план 4 шлёт алерт открытия — если бы двое получили created=true, был бы дубль.
func TestRegressionOpenConcurrentOnlyOneWins(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "reg-race")

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	createdCount := 0
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, created, err := svc.Open(ctx, pid, "endpoint_p95", "GET /race", "duration", 100, 250, false)
			if err != nil {
				errs[i] = err
				return
			}
			if created {
				mu.Lock()
				createdCount++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
	if createdCount != 1 {
		t.Fatalf("createdCount = %d, want exactly 1", createdCount)
	}

	var openCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM perf_regressions WHERE project_id = $1 AND status = 'open'", pid).Scan(&openCount); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("open count = %d, want 1", openCount)
	}
}

// TestRegressionOpenIdempotent: первый Open создаёт (created=true), второй по той
// же цели — нет (created=false), строка одна.
func TestRegressionOpenIdempotent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "reg-open")

	rec1, created1, err := svc.Open(ctx, pid, "endpoint_p95", "GET /api/users", "duration", 100, 250, false)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	if !created1 {
		t.Fatalf("Open 1: created = false, want true")
	}
	if rec1.ID == 0 || rec1.Status != "open" {
		t.Fatalf("Open 1: unexpected %+v", rec1)
	}
	if rec1.BaselineValue != 100 || rec1.PeakValue != 250 || rec1.CurrentValue != 250 {
		t.Fatalf("Open 1: values = base %v peak %v cur %v, want 100/250/250", rec1.BaselineValue, rec1.PeakValue, rec1.CurrentValue)
	}

	rec2, created2, err := svc.Open(ctx, pid, "endpoint_p95", "GET /api/users", "duration", 999, 999, false)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	if created2 {
		t.Fatalf("Open 2: created = true, want false")
	}
	if rec2.ID != rec1.ID {
		t.Fatalf("Open 2: id = %d, want %d (existing)", rec2.ID, rec1.ID)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM perf_regressions WHERE project_id = $1", pid).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}
}

// TestRegressionAcknowledge — B4: Acknowledge на открытом инциденте ставит
// acknowledged_at/acknowledged_by и возвращает ok=true; повторный вызов и
// вызов на закрытом инциденте — идемпотентно ok=false. scan (List) после ack
// отдаёт заполненные поля, до ack — nil.
func TestRegressionAcknowledge(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx := context.Background()
	pid := newPerfProject(t, pool, "reg-ack")

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "trace-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	rec, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /ack", "duration", 100, 250, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	list, err := svc.List(ctx, pid, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil || list[0].AcknowledgedBy != nil {
		t.Fatalf("до Acknowledge: list=%+v err=%v, want AcknowledgedAt/By nil", list, err)
	}

	ok, err := svc.Acknowledge(ctx, rec.ID, pid, userID)
	if err != nil || !ok {
		t.Fatalf("Acknowledge = (%v,%v), want (true,nil)", ok, err)
	}

	list, err = svc.List(ctx, pid, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt == nil {
		t.Fatalf("после Acknowledge: list=%+v err=%v, want AcknowledgedAt заполнено", list, err)
	}
	if list[0].AcknowledgedBy == nil || *list[0].AcknowledgedBy != userID {
		t.Fatalf("после Acknowledge: AcknowledgedBy = %v, want %d", list[0].AcknowledgedBy, userID)
	}

	// Повторный ack — идемпотентно ok=false.
	if ok2, err := svc.Acknowledge(ctx, rec.ID, pid, userID); err != nil || ok2 {
		t.Fatalf("повторный Acknowledge = (%v,%v), want (false,nil)", ok2, err)
	}

	// Acknowledge закрытого инцидента — ok=false.
	if _, err := svc.Resolve(ctx, rec.ID, 50); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	closedRec, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /ack2", "duration", 100, 250, false)
	if err != nil {
		t.Fatalf("Open closedRec: %v", err)
	}
	if _, err := svc.Resolve(ctx, closedRec.ID, 10); err != nil {
		t.Fatalf("resolve closedRec: %v", err)
	}
	if okClosed, err := svc.Acknowledge(ctx, closedRec.ID, pid, userID); err != nil || okClosed {
		t.Fatalf("Acknowledge закрытого = (%v,%v), want (false,nil)", okClosed, err)
	}
}

// TestRegressionAcknowledgeForeignProject — project_id — часть WHERE
// Acknowledge (defense-in-depth, зеркало uptime.DeleteWindow, B3).
func TestRegressionAcknowledgeForeignProject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx := context.Background()
	pid := newPerfProject(t, pool, "reg-ack-foreign")
	otherPID := newPerfProject(t, pool, "reg-ack-foreign-2")

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "trace-ack-foreign@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	rec, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /ack-foreign", "duration", 100, 250, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if ok, err := svc.Acknowledge(ctx, rec.ID, otherPID, userID); err != nil || ok {
		t.Fatalf("Acknowledge с чужим project_id = (%v,%v), want (false,nil)", ok, err)
	}

	list, err := svc.List(ctx, pid, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil {
		t.Fatalf("после чужого Acknowledge: list=%+v err=%v, want AcknowledgedAt nil", list, err)
	}
}

// TestRegressionBump: current обновляется, peak = max(peak, current).
func TestRegressionBump(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "reg-bump")
	rec, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /x", "duration", 100, 200, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Рост: peak и current идут вверх.
	if err := svc.Bump(ctx, rec.ID, 300); err != nil {
		t.Fatalf("Bump up: %v", err)
	}
	got, ok, err := svc.OpenFor(ctx, pid, "GET /x", "duration")
	if err != nil || !ok {
		t.Fatalf("OpenFor: ok=%v err=%v", ok, err)
	}
	if got.CurrentValue != 300 || got.PeakValue != 300 {
		t.Fatalf("after up: cur %v peak %v, want 300/300", got.CurrentValue, got.PeakValue)
	}

	// Спад: current вниз, peak держит максимум.
	if err := svc.Bump(ctx, rec.ID, 150); err != nil {
		t.Fatalf("Bump down: %v", err)
	}
	got, _, err = svc.OpenFor(ctx, pid, "GET /x", "duration")
	if err != nil {
		t.Fatalf("OpenFor 2: %v", err)
	}
	if got.CurrentValue != 150 || got.PeakValue != 300 {
		t.Fatalf("after down: cur %v peak %v, want 150/300", got.CurrentValue, got.PeakValue)
	}
}

// TestRegressionResolveIdempotent: Resolve закрывает (status/resolved_at),
// повторный Resolve → false, после закрытия можно открыть новый по той же цели.
func TestRegressionResolveIdempotent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "reg-resolve")
	rec, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /y", "duration", 100, 200, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ok, err := svc.Resolve(ctx, rec.ID, 110)
	if err != nil {
		t.Fatalf("Resolve 1: %v", err)
	}
	if !ok {
		t.Fatalf("Resolve 1: ok = false, want true")
	}

	var status string
	var resolvedAt *time.Time
	var current float64
	if err := pool.QueryRow(ctx,
		"SELECT status, resolved_at, current_value FROM perf_regressions WHERE id = $1", rec.ID).
		Scan(&status, &resolvedAt, &current); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "resolved" || resolvedAt == nil || current != 110 {
		t.Fatalf("after resolve: status %q resolvedAt %v cur %v", status, resolvedAt, current)
	}

	ok2, err := svc.Resolve(ctx, rec.ID, 120)
	if err != nil {
		t.Fatalf("Resolve 2: %v", err)
	}
	if ok2 {
		t.Fatalf("Resolve 2: ok = true, want false (already resolved)")
	}

	// После закрытия частичный индекс свободен — новый open по той же цели.
	rec3, created3, err := svc.Open(ctx, pid, "endpoint_p95", "GET /y", "duration", 100, 300, false)
	if err != nil {
		t.Fatalf("Open after resolve: %v", err)
	}
	if !created3 {
		t.Fatalf("Open after resolve: created = false, want true")
	}
	if rec3.ID == rec.ID {
		t.Fatalf("Open after resolve: reused old id %d", rec.ID)
	}
}

// TestRegressionOpenFor: находит открытый, не находит закрытый.
func TestRegressionOpenFor(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "reg-openfor")

	if _, found, err := svc.OpenFor(ctx, pid, "GET /z", "duration"); err != nil || found {
		t.Fatalf("OpenFor before any: found=%v err=%v", found, err)
	}

	rec, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /z", "duration", 100, 200, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, found, err := svc.OpenFor(ctx, pid, "GET /z", "duration")
	if err != nil || !found || got.ID != rec.ID {
		t.Fatalf("OpenFor open: found=%v got=%+v err=%v", found, got, err)
	}

	if _, err := svc.Resolve(ctx, rec.ID, 100); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, found, err := svc.OpenFor(ctx, pid, "GET /z", "duration"); err != nil || found {
		t.Fatalf("OpenFor after resolve: found=%v err=%v", found, err)
	}
}

// TestRegressionMarkNotified: open=true → notified_open, open=false →
// notified_close; неизвестный id → ErrNotFound.
func TestRegressionMarkNotified(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "reg-notify")
	rec, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /n", "duration", 100, 200, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := svc.MarkNotified(ctx, rec.ID, true); err != nil {
		t.Fatalf("MarkNotified open: %v", err)
	}
	if err := svc.MarkNotified(ctx, rec.ID, false); err != nil {
		t.Fatalf("MarkNotified close: %v", err)
	}
	got, _, err := svc.OpenFor(ctx, pid, "GET /n", "duration")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !got.NotifiedOpen || !got.NotifiedClose {
		t.Fatalf("MarkNotified did not persist: %+v", got)
	}

	if err := svc.MarkNotified(ctx, 999999999, true); !errors.Is(err, trace.ErrNotFound) {
		t.Fatalf("MarkNotified unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestRegressionPartialIndex: две РАЗНЫЕ цели одного проекта → два инцидента; та
// же цель дважды open → одна строка (частичный уникальный индекс).
func TestRegressionPartialIndex(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "reg-index")

	// Разные метрики одной цели — тоже разные цели индекса (project,target,metric).
	if _, c, err := svc.Open(ctx, pid, "endpoint_p95", "GET /a", "duration", 1, 2, false); err != nil || !c {
		t.Fatalf("Open a/duration: c=%v err=%v", c, err)
	}
	if _, c, err := svc.Open(ctx, pid, "webvital_p75", "GET /a", "lcp", 1, 2, false); err != nil || !c {
		t.Fatalf("Open a/lcp: c=%v err=%v", c, err)
	}
	if _, c, err := svc.Open(ctx, pid, "endpoint_p95", "GET /b", "duration", 1, 2, false); err != nil || !c {
		t.Fatalf("Open b/duration: c=%v err=%v", c, err)
	}
	// Дубль по (a,duration) — не создаётся.
	if _, c, err := svc.Open(ctx, pid, "endpoint_p95", "GET /a", "duration", 1, 2, false); err != nil || c {
		t.Fatalf("Open a/duration dup: c=%v err=%v, want created=false", c, err)
	}

	var openCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM perf_regressions WHERE project_id = $1 AND status = 'open'", pid).Scan(&openCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if openCount != 3 {
		t.Fatalf("open count = %d, want 3", openCount)
	}
}

// TestRegressionList: регрессии проекта, свежайшие первыми.
func TestRegressionList(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "reg-list")

	r1, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /1", "duration", 1, 2, false)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	r2, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /2", "duration", 1, 2, false)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}

	list, err := svc.List(ctx, pid, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != r2.ID || list[1].ID != r1.ID {
		t.Fatalf("List = %+v, want [r2 r1] freshest first", list)
	}
}
