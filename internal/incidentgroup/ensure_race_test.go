package incidentgroup_test

import (
	"context"
	"sync"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestEnsureGroupConcurrentRaceYieldsExactlyOneGroup — R2b/W37: EnsureGroup
// полагается на INSERT .. ON CONFLICT (root_source, root_incident_id) DO
// NOTHING + дочитывание проигравшими победителя (комментарий в group.go —
// «тот же приём, что и host.IncidentService.Open»). До этого теста гонка
// проверялась только последовательной идемпотентностью (вызов за вызовом в
// одной горутине) — реальная параллельность ни разу не запускалась. Здесь N
// горутин одновременно бьются за один и тот же корень: должна выжить ровно
// одна строка группы, и ВСЕ горутины обязаны получить один и тот же id
// (не только «не упасть с ошибкой»).
func TestEnsureGroupConcurrentRaceYieldsExactlyOneGroup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)

	store := incidentgroup.NewStore(pool)

	const n = 24
	ids := make([]int64, n)
	errs := make([]error, n)

	var ready sync.WaitGroup
	ready.Add(n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
			ids[i] = g.ID
			errs[i] = err
		}(i)
	}
	ready.Wait() // все горутины на старте, до первого EnsureGroup — сжимаем окно гонки.
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: EnsureGroup: %v", i, err)
		}
	}
	want := ids[0]
	if want == 0 {
		t.Fatalf("group id must not be zero")
	}
	for i, id := range ids {
		if id != want {
			t.Fatalf("goroutine %d got group id %d, want %d (all callers must converge on the same winner)", i, id, want)
		}
	}

	var cnt int64
	mustScan(t, pool, &cnt,
		`SELECT count(*) FROM incident_groups WHERE root_source = 'host' AND root_incident_id = $1`, rootInc)
	if cnt != 1 {
		t.Fatalf("concurrent EnsureGroup race must create exactly one group row, got %d", cnt)
	}
}
