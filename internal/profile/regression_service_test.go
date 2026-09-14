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

	r, created, err := svc.Open(ctx, pid, "api", "cpu", "slow", 0.1, 0.3, false)
	if err != nil || !created {
		t.Fatalf("open = (%+v,%v,%v)", r, created, err)
	}
	if _, c2, _ := svc.Open(ctx, pid, "api", "cpu", "slow", 0.1, 0.4, false); c2 {
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
	if _, c3, _ := svc.Open(ctx, pid, "api", "cpu", "slow", 0.1, 0.5, false); !c3 {
		t.Fatal("open after resolve must be created=true")
	}
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

func TestRegressionServiceAcknowledge(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := profile.NewRegressionService(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", t.Name()+"-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	r, _, err := svc.Open(ctx, pid, "api", "cpu", "slow", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	list, err := svc.List(ctx, pid, "open", 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil || list[0].AcknowledgedBy != nil {
		t.Fatalf("до Acknowledge: list=%+v err=%v, want AcknowledgedAt/By nil", list, err)
	}

	ok, err := svc.Acknowledge(ctx, r.ID, pid, userID)
	if err != nil || !ok {
		t.Fatalf("Acknowledge = (%v,%v), want (true,nil)", ok, err)
	}

	list, err = svc.List(ctx, pid, "open", 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt == nil {
		t.Fatalf("после Acknowledge: list=%+v err=%v, want AcknowledgedAt заполнено", list, err)
	}
	if list[0].AcknowledgedBy == nil || *list[0].AcknowledgedBy != userID {
		t.Fatalf("после Acknowledge: AcknowledgedBy = %v, want %d", list[0].AcknowledgedBy, userID)
	}

	if ok2, err := svc.Acknowledge(ctx, r.ID, pid, userID); err != nil || ok2 {
		t.Fatalf("повторный Acknowledge = (%v,%v), want (false,nil)", ok2, err)
	}

	if _, err := svc.Resolve(ctx, r.ID, 0.11); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if okClosed, err := svc.Acknowledge(ctx, r.ID, pid, userID); err != nil || okClosed {
		t.Fatalf("Acknowledge закрытого = (%v,%v), want (false,nil)", okClosed, err)
	}
}

func TestRegressionServiceAcknowledgeForeignProject(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := profile.NewRegressionService(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)
	// вторым seedProject(t, pool) нельзя: он ключует email/org/project по t.Name(),
	// повтор упёрся бы в users_email_key.
	var otherPID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name, platform) SELECT org_id, $2, $2, 'go' FROM projects WHERE id = $1 RETURNING id",
		pid, t.Name()+"-other").Scan(&otherPID); err != nil {
		t.Fatalf("insert other project: %v", err)
	}

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", t.Name()+"-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	r, _, err := svc.Open(ctx, pid, "api", "cpu", "slow", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if ok, err := svc.Acknowledge(ctx, r.ID, otherPID, userID); err != nil || ok {
		t.Fatalf("Acknowledge с чужим project_id = (%v,%v), want (false,nil)", ok, err)
	}

	list, err := svc.List(ctx, pid, "open", 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil {
		t.Fatalf("после чужого Acknowledge: list=%+v err=%v, want AcknowledgedAt nil", list, err)
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
			_, c, err := svc.Open(ctx, pid, "api", "cpu", "hot", 0.1, 0.2+float64(i)/100, false)
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

func TestRegressionServiceOpenForService(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := profile.NewRegressionService(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)

	f1, _, err := svc.Open(ctx, pid, "api", "cpu", "f1", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("open f1: %v", err)
	}
	f3, _, err := svc.Open(ctx, pid, "api", "cpu", "f3", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("open f3: %v", err)
	}
	if _, _, err := svc.Open(ctx, pid, "web", "cpu", "f2", 0.1, 0.3, false); err != nil {
		t.Fatalf("open f2/web: %v", err)
	}
	if _, _, err := svc.Open(ctx, pid, "api", "alloc", "f2", 0.1, 0.3, false); err != nil {
		t.Fatalf("open f2/alloc: %v", err)
	}
	f4, _, err := svc.Open(ctx, pid, "api", "cpu", "f4", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("open f4: %v", err)
	}
	if ok, err := svc.Resolve(ctx, f4.ID, 0.1); err != nil || !ok {
		t.Fatalf("resolve f4 = (%v,%v)", ok, err)
	}

	got, err := svc.OpenForService(ctx, pid, "api", "cpu")
	if err != nil {
		t.Fatalf("OpenForService: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("OpenForService = %d entries (%v), want exactly f1 and f3", len(got), keysOf(got))
	}
	if got["f1"].ID != f1.ID || got["f3"].ID != f3.ID {
		t.Fatalf("OpenForService ids = f1:%d f3:%d, want f1:%d f3:%d", got["f1"].ID, got["f3"].ID, f1.ID, f3.ID)
	}
	if _, ok := got["f4"]; ok {
		t.Fatal("resolved regression f4 returned as open")
	}
	if _, ok := got["f2"]; ok {
		t.Fatal("regression of another service/type returned under our key")
	}

	got, err = svc.OpenForService(ctx, pid, "web", "cpu")
	if err != nil || len(got) != 1 {
		t.Fatalf("OpenForService(web/cpu) = %v err=%v, want only f2", keysOf(got), err)
	}

	got, err = svc.OpenForService(ctx, pid, "api", "gpu")
	if err != nil || len(got) != 0 {
		t.Fatalf("OpenForService(api/gpu) = %v err=%v, want empty (no such profile type)", got, err)
	}
}

func TestRegressionServiceOpenServices(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := profile.NewRegressionService(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)

	if _, _, err := svc.Open(ctx, pid, "api", "cpu", "f1", 0.1, 0.3, false); err != nil {
		t.Fatalf("open f1: %v", err)
	}
	gone, _, err := svc.Open(ctx, pid, "web", "cpu", "f2", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("open f2/web: %v", err)
	}

	services, err := svc.OpenServices(ctx)
	if err != nil {
		t.Fatalf("OpenServices: %v", err)
	}
	want := map[profile.ProjectService]bool{
		{ProjectID: pid, Service: "api", Type: "cpu"}: true,
		{ProjectID: pid, Service: "web", Type: "cpu"}: true,
	}
	if len(services) != len(want) {
		t.Fatalf("OpenServices = %v, want %v", services, want)
	}
	for _, ps := range services {
		if !want[ps] {
			t.Fatalf("OpenServices returned unexpected %+v", ps)
		}
	}

	if ok, err := svc.Resolve(ctx, gone.ID, 0.1); err != nil || !ok {
		t.Fatalf("resolve f2/web = (%v,%v)", ok, err)
	}
	services, err = svc.OpenServices(ctx)
	if err != nil || len(services) != 1 || services[0].Service != "api" {
		t.Fatalf("OpenServices после resolve = %v err=%v, want only api/cpu", services, err)
	}
}

func keysOf(m map[string]profile.Regression) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
