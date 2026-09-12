package alert_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Конкурентный ClaimSuppressed обязан уйти с пустым результатом, а не
// забрать и обнулить те же строки повторно (FOR UPDATE SKIP LOCKED).
func TestAlertClaimSuppressedConcurrentClaimsWinExactlyOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx := context.Background()

	pid := newEvalProject(t, pool, "claim-suppressed-race")
	if _, err := pool.Exec(ctx,
		`INSERT INTO alert_project_budget (project_id, window_start, sent, suppressed, allowed)
		 VALUES ($1, now() - interval '2 hours', 5, 7, false)`, pid); err != nil {
		t.Fatalf("seed alert_project_budget: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners int
	var totalSuppressed int
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			batches, err := svc.ClaimSuppressed(ctx, 10)
			if err != nil {
				errs[i] = err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, b := range batches {
				if b.ProjectID == pid {
					winners++
					totalSuppressed += b.Suppressed
				}
			}
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent ClaimSuppressed: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (20 конкурентных ClaimSuppressed по одному проекту)", winners)
	}
	if totalSuppressed != 7 {
		t.Fatalf("totalSuppressed = %d, want 7 (ни разослано дважды, ни утеряно)", totalSuppressed)
	}

	var suppressed int
	if err := pool.QueryRow(ctx,
		"SELECT suppressed FROM alert_project_budget WHERE project_id = $1", pid).Scan(&suppressed); err != nil {
		t.Fatalf("select suppressed: %v", err)
	}
	if suppressed != 0 {
		t.Fatalf("suppressed после клейма = %d, want 0", suppressed)
	}
}

// Задержка внутри UPDATE форсирует настоящее пересечение — короткий UPDATE
// без неё почти всегда сериализуется раньше, чем горутины столкнутся.
func TestAlertClaimSuppressedOverlappingUpdateWinsOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx := context.Background()

	pid := newEvalProject(t, pool, "claim-suppressed-overlap")
	if _, err := pool.Exec(ctx,
		`INSERT INTO alert_project_budget (project_id, window_start, sent, suppressed, allowed)
		 VALUES ($1, now() - interval '2 hours', 5, 7, false)`, pid); err != nil {
		t.Fatalf("seed alert_project_budget: %v", err)
	}

	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION test_delay_budget_update()
		RETURNS trigger AS $$ BEGIN PERFORM pg_sleep(1); RETURN NEW; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create delay function: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_delay_budget_update
		BEFORE UPDATE ON alert_project_budget FOR EACH ROW
		EXECUTE FUNCTION test_delay_budget_update()`); err != nil {
		t.Fatalf("create delay trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			"DROP TRIGGER IF EXISTS test_delay_budget_update ON alert_project_budget"); err != nil {
			t.Errorf("drop delay trigger: %v", err)
		}
	})

	var wg sync.WaitGroup
	results := make([][]alert.SuppressedBatch, 2)
	errs := make([]error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], errs[0] = svc.ClaimSuppressed(ctx, 10)
	}()
	time.Sleep(300 * time.Millisecond) // A обязана быть внутри триггера (держит лок строки) к этому моменту
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[1], errs[1] = svc.ClaimSuppressed(ctx, 10)
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("ClaimSuppressed[%d]: %v", i, err)
		}
	}

	var winners int
	for _, batches := range results {
		for _, b := range batches {
			if b.ProjectID == pid {
				winners++
			}
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (B должна была SKIP LOCKED уйти пустой, пока A держит строку в UPDATE)", winners)
	}
}
