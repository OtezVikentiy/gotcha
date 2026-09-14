package slo_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Конкурентные OpenIncident обязаны сойтись на одном инциденте, а не
// создать по одному каждый (частичный индекс slo_incidents_one_open_idx).
func TestSLOStoreOpenIncidentConcurrentOnlyOneWins(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	created, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "concurrency", Kind: slo.SLIAvailability, Target: 0.99,
		WindowDays: 30, Transaction: "GET /", BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var createdCount int
	winnerIDs := map[int64]struct{}{}
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inc, wasCreated, err := st.OpenIncident(ctx, created.ID, pid, 20.0, nil, false)
			if err != nil {
				errs[i] = err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if wasCreated {
				createdCount++
			}
			winnerIDs[inc.ID] = struct{}{}
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent OpenIncident: %v", err)
		}
	}
	if createdCount != 1 {
		t.Fatalf("createdCount = %d, want exactly 1", createdCount)
	}
	if len(winnerIDs) != 1 {
		t.Fatalf("winnerIDs = %v, want ровно один общий id инцидента", winnerIDs)
	}
}

// Задержка внутри INSERT форсирует настоящее столкновение на нём, а не на
// предшествующем SELECT — иначе ветка unique-violation в OpenIncident не проверяется.
func TestSLOStoreOpenIncidentOverlappingInsertRecoversFromConflict(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	created, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "overlap", Kind: slo.SLIAvailability, Target: 0.99,
		WindowDays: 30, Transaction: "GET /overlap", BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION test_delay_slo_incident_insert()
		RETURNS trigger AS $$ BEGIN PERFORM pg_sleep(1); RETURN NEW; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create delay function: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER test_delay_slo_incident_insert
		BEFORE INSERT ON slo_incidents FOR EACH ROW
		EXECUTE FUNCTION test_delay_slo_incident_insert()`); err != nil {
		t.Fatalf("create delay trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			"DROP TRIGGER IF EXISTS test_delay_slo_incident_insert ON slo_incidents"); err != nil {
			t.Errorf("drop delay trigger: %v", err)
		}
	})

	var wg sync.WaitGroup
	var results [2]slo.Incident
	var createdFlags [2]bool
	var errs [2]error
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], createdFlags[0], errs[0] = st.OpenIncident(ctx, created.ID, pid, 20.0, nil, false)
	}()
	time.Sleep(300 * time.Millisecond) // A обязана быть внутри триггера (до фактической записи строки) к этому моменту
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[1], createdFlags[1], errs[1] = st.OpenIncident(ctx, created.ID, pid, 20.0, nil, false)
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("OpenIncident[%d]: %v", i, err)
		}
	}
	if createdFlags[0] == createdFlags[1] {
		t.Fatalf("createdFlags = %v, want ровно один true и один false (unique-violation должен быть пойман и перечитан)", createdFlags)
	}
	if results[0].ID != results[1].ID {
		t.Fatalf("incident ids = %d и %d, want один общий", results[0].ID, results[1].ID)
	}
}
