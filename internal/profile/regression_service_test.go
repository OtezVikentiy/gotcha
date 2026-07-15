package profile_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var uid, orgID, projID int64
	pool.QueryRow(ctx, "INSERT INTO users (email,password_hash) VALUES ($1,'x') RETURNING id", t.Name()+"@e.com").Scan(&uid)
	pool.QueryRow(ctx, "INSERT INTO organizations (slug,name,event_quota) VALUES ($1,$1,1000000) RETURNING id", t.Name()+"o").Scan(&orgID)
	pool.QueryRow(ctx, "INSERT INTO projects (org_id,slug,name,platform) VALUES ($1,$2,$2,'go') RETURNING id", orgID, t.Name()+"p").Scan(&projID)
	return projID
}

func TestRegressionServiceOpenClose(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := profile.NewRegressionService(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)

	r, created, err := svc.Open(ctx, pid, "api", "cpu", "slow", 0.1, 0.3)
	if err != nil || !created {
		t.Fatalf("open = (%+v,%v,%v)", r, created, err)
	}
	if _, c2, _ := svc.Open(ctx, pid, "api", "cpu", "slow", 0.1, 0.4); c2 {
		t.Fatal("second open must be created=false")
	}
	if err := svc.Bump(ctx, r.ID, 0.4); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if ok, _ := svc.Resolve(ctx, r.ID, 0.11); !ok {
		t.Fatal("resolve must be ok=true")
	}
	if ok, _ := svc.Resolve(ctx, r.ID, 0.11); ok {
		t.Fatal("second resolve must be ok=false")
	}
	if _, c3, _ := svc.Open(ctx, pid, "api", "cpu", "slow", 0.1, 0.5); !c3 {
		t.Fatal("open after resolve must be created=true")
	}
	// List фильтры: 1 open + 1 resolved = 2 all.
	if all, _ := svc.List(ctx, pid, "all", 10); len(all) != 2 {
		t.Fatalf("all = %d, want 2", len(all))
	}
	if op, _ := svc.List(ctx, pid, "open", 10); len(op) != 1 {
		t.Fatalf("open list = %d, want 1", len(op))
	}
	if rs, _ := svc.List(ctx, pid, "resolved", 10); len(rs) != 1 {
		t.Fatalf("resolved list = %d, want 1", len(rs))
	}
}

func TestRegressionOpenConcurrentOnlyOneWins(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := profile.NewRegressionService(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)

	const n = 20
	var wg sync.WaitGroup
	created := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, c, err := svc.Open(ctx, pid, "api", "cpu", "hot", 0.1, 0.2+float64(i)/100)
			if err != nil {
				t.Errorf("open %d: %v", i, err)
			}
			created[i] = c
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, c := range created {
		if c {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent opens created=true count = %d, want 1", wins)
	}
}
