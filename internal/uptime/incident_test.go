package uptime_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestOpenIncidentReturnsExistingOnSecondCall(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)

	inc1, created1, err := svc.OpenIncident(ctx, mon.ID, "http 500", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident 1: %v", err)
	}
	if !created1 {
		t.Fatalf("OpenIncident 1: created = false, want true")
	}
	if inc1.ID == 0 || inc1.Cause != "http 500" {
		t.Fatalf("OpenIncident 1: unexpected incident %+v", inc1)
	}

	inc2, created2, err := svc.OpenIncident(ctx, mon.ID, "http 500 again", []string{"eu"}, false)
	if err != nil {
		t.Fatalf("OpenIncident 2: %v", err)
	}
	if created2 {
		t.Fatalf("OpenIncident 2: created = true, want false")
	}
	if inc2.ID != inc1.ID {
		t.Fatalf("OpenIncident 2: id = %d, want %d (existing)", inc2.ID, inc1.ID)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM incidents WHERE monitor_id = $1", mon.ID).Scan(&count); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if count != 1 {
		t.Fatalf("incident count = %d, want 1", count)
	}
}

func TestOpenIncidentConcurrentOnlyOneWins(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var createdCount int
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, created, err := svc.OpenIncident(ctx, mon.ID, "concurrent down", nil, false)
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
			t.Fatalf("concurrent OpenIncident: %v", err)
		}
	}
	if createdCount != 1 {
		t.Fatalf("createdCount = %d, want exactly 1", createdCount)
	}

	var openCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incidents WHERE monitor_id = $1 AND resolved_at IS NULL", mon.ID).Scan(&openCount); err != nil {
		t.Fatalf("count open incidents: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("openCount = %d, want 1 (unique partial index must prevent duplicates)", openCount)
	}
}

func TestResolveIncidentIdempotent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)

	if _, _, err := svc.OpenIncident(ctx, mon.ID, "cause", []string{"local"}, false); err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	resolved, ok, err := svc.ResolveIncident(ctx, mon.ID, at)
	if err != nil {
		t.Fatalf("ResolveIncident 1: %v", err)
	}
	if !ok {
		t.Fatalf("ResolveIncident 1: ok = false, want true")
	}
	if resolved.ResolvedAt == nil || !resolved.ResolvedAt.Equal(at) {
		t.Fatalf("ResolveIncident 1: ResolvedAt = %v, want %v", resolved.ResolvedAt, at)
	}

	_, ok2, err := svc.ResolveIncident(ctx, mon.ID, time.Now())
	if err != nil {
		t.Fatalf("ResolveIncident 2: %v", err)
	}
	if ok2 {
		t.Fatalf("ResolveIncident 2: ok = true, want false (nothing open)")
	}

	// Opening again after resolve must create a brand new incident (the
	// partial unique index only blocks a second concurrently-open one).
	inc3, created3, err := svc.OpenIncident(ctx, mon.ID, "new cause", nil, false)
	if err != nil {
		t.Fatalf("OpenIncident after resolve: %v", err)
	}
	if !created3 {
		t.Fatalf("OpenIncident after resolve: created = false, want true")
	}
	if inc3.ID == resolved.ID {
		t.Fatalf("OpenIncident after resolve: reused old incident id")
	}
}

func TestOpenIncidentForAndListings(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	monA := createMonitor(t, svc, pid, 3, 2)
	monB := createMonitor(t, svc, pid, 3, 2)

	if _, found, err := svc.OpenIncidentFor(ctx, monA.ID); err != nil || found {
		t.Fatalf("OpenIncidentFor before any incident: found=%v err=%v", found, err)
	}

	incA, _, err := svc.OpenIncident(ctx, monA.ID, "a down", nil, false)
	if err != nil {
		t.Fatalf("OpenIncident A: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	incB, _, err := svc.OpenIncident(ctx, monB.ID, "b down", nil, false)
	if err != nil {
		t.Fatalf("OpenIncident B: %v", err)
	}

	found, ok, err := svc.OpenIncidentFor(ctx, monA.ID)
	if err != nil {
		t.Fatalf("OpenIncidentFor: %v", err)
	}
	if !ok || found.ID != incA.ID {
		t.Fatalf("OpenIncidentFor: found=%+v ok=%v, want incA", found, ok)
	}

	list, err := svc.Incidents(ctx, pid, 10)
	if err != nil {
		t.Fatalf("Incidents: %v", err)
	}
	if len(list) != 2 || list[0].ID != incB.ID || list[1].ID != incA.ID {
		t.Fatalf("Incidents = %+v, want [incB incA] freshest first", list)
	}

	listA, err := svc.IncidentsForMonitor(ctx, monA.ID, 10)
	if err != nil {
		t.Fatalf("IncidentsForMonitor: %v", err)
	}
	if len(listA) != 1 || listA[0].ID != incA.ID {
		t.Fatalf("IncidentsForMonitor = %+v", listA)
	}

	if err := svc.MarkNotified(ctx, incA.ID, true); err != nil {
		t.Fatalf("MarkNotified open: %v", err)
	}
	if err := svc.MarkNotified(ctx, incA.ID, false); err != nil {
		t.Fatalf("MarkNotified close: %v", err)
	}
	got, _, err := svc.OpenIncidentFor(ctx, monA.ID)
	if err != nil {
		t.Fatalf("OpenIncidentFor after notify: %v", err)
	}
	if !got.NotifiedOpen || !got.NotifiedClose {
		t.Fatalf("MarkNotified did not persist: %+v", got)
	}

	if err := svc.MarkNotified(ctx, 999999999, true); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("MarkNotified unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestIncidentsForMonitorsBatchRespectsPerMonitorLimitAndIsolation:
// IncidentsForMonitorsBatch обязан отдавать limit инцидентов НА КАЖДЫЙ
// монитор набора, а не limit суммарно на весь набор (см. докблок метода про
// row_number() PARTITION BY monitor_id вместо общего LIMIT), и не путать
// инциденты разных мониторов — тот же риск мис-ключевания, ради которого в
// query_test.go завёден BatchParity с разными данными на двух мониторах.
func TestIncidentsForMonitorsBatchRespectsPerMonitorLimitAndIsolation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	monMany := createMonitor(t, svc, pid, 3, 2)
	monFew := createMonitor(t, svc, pid, 3, 2)
	monNone := createMonitor(t, svc, pid, 3, 2)

	// monMany получает три инцидента подряд (открыт-закрыт трижды) —
	// больше, чем limit=2 в самом запросе ниже.
	var manyIDs []int64
	for i := 0; i < 3; i++ {
		inc, _, err := svc.OpenIncident(ctx, monMany.ID, "boom", nil, false)
		if err != nil {
			t.Fatalf("OpenIncident monMany #%d: %v", i, err)
		}
		manyIDs = append(manyIDs, inc.ID)
		if _, _, err := svc.ResolveIncident(ctx, monMany.ID, time.Now()); err != nil {
			t.Fatalf("ResolveIncident monMany #%d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond) // разводим started_at по времени
	}
	// monFew получает ровно один — меньше limit, не должен дотягиваться до
	// чужих (monMany) инцидентов.
	incFew, _, err := svc.OpenIncident(ctx, monFew.ID, "boom", nil, false)
	if err != nil {
		t.Fatalf("OpenIncident monFew: %v", err)
	}

	const limit = 2
	got, err := svc.IncidentsForMonitorsBatch(ctx, []int64{monMany.ID, monFew.ID, monNone.ID}, limit)
	if err != nil {
		t.Fatalf("IncidentsForMonitorsBatch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (карта заполнена для всех запрошенных id)", len(got))
	}

	// monMany: limit=2 из трёх, freshest first — два последних по времени
	// открытия (последний свежее, чем первый).
	if len(got[monMany.ID]) != limit {
		t.Fatalf("got[monMany] = %d incidents, want %d (свой лимит, не общий на набор)", len(got[monMany.ID]), limit)
	}
	if got[monMany.ID][0].ID != manyIDs[2] || got[monMany.ID][1].ID != manyIDs[1] {
		t.Fatalf("got[monMany] = %+v, want freshest-first [%d %d]", got[monMany.ID], manyIDs[2], manyIDs[1])
	}
	for _, inc := range got[monMany.ID] {
		if inc.MonitorID != monMany.ID {
			t.Fatalf("got[monMany] содержит инцидент чужого монитора: %+v", inc)
		}
	}

	// monFew: один инцидент, свой лимит его не срезал и соседний monMany не
	// вытеснил его из выдачи (что случилось бы с общим LIMIT после ORDER BY
	// без PARTITION BY).
	if len(got[monFew.ID]) != 1 || got[monFew.ID][0].ID != incFew.ID {
		t.Fatalf("got[monFew] = %+v, want [%d]", got[monFew.ID], incFew.ID)
	}

	// monNone: запрошен, инцидентов нет — присутствует в карте с nil-слайсом.
	if got[monNone.ID] != nil {
		t.Fatalf("got[monNone] = %v, want nil", got[monNone.ID])
	}

	// Паритет с IncidentsForMonitor по каждому монитору отдельно.
	single, err := svc.IncidentsForMonitor(ctx, monMany.ID, limit)
	if err != nil {
		t.Fatalf("IncidentsForMonitor(monMany): %v", err)
	}
	if len(single) != len(got[monMany.ID]) {
		t.Fatalf("IncidentsForMonitor(monMany) = %d, IncidentsForMonitorsBatch = %d", len(single), len(got[monMany.ID]))
	}
	for i := range single {
		if single[i].ID != got[monMany.ID][i].ID {
			t.Errorf("IncidentsForMonitorsBatch[monMany][%d] = %+v, IncidentsForMonitor = %+v", i, got[monMany.ID][i], single[i])
		}
	}
}

// TestClearSuppressedByDepIsIdempotent (M3, финревью волны 1 аудита перед
// 1.0): докблок ClearSuppressedByDep называет повторный вызов идемпотентным
// — второй Clear на уже снятом подавлении обязан вернуть nil, а не
// ErrNotFound (раньше отдавал его при RowsAffected()==0), симметрично
// host.IncidentService.ClearSuppressed, который RowsAffected не смотрит
// вовсе. На гонке двух реплик Detector.settleHeldIncident проигравшая
// раньше получала ErrNotFound → Warn → return и пропускала settle этого
// тика; мутация — вернуть проверку RowsAffected()==0/ErrNotFound — обязана
// уронить второй вызов ниже.
func TestClearSuppressedByDepIsIdempotent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	inc, _, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if err := svc.MarkSuppressedByDep(ctx, inc.ID); err != nil {
		t.Fatalf("MarkSuppressedByDep: %v", err)
	}

	if err := svc.ClearSuppressedByDep(ctx, inc.ID); err != nil {
		t.Fatalf("первый ClearSuppressedByDep: %v", err)
	}
	// Повторный вызов на уже снятом подавлении — идемпотентный no-op.
	if err := svc.ClearSuppressedByDep(ctx, inc.ID); err != nil {
		t.Fatalf("повторный ClearSuppressedByDep = %v, want nil (идемпотентно)", err)
	}
}
