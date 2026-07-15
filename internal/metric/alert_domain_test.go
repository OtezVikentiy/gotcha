package metric_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var uid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", t.Name()+"@e.com").Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,$1,1000000) RETURNING id", t.Name()+"-o").Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name, platform) VALUES ($1,$2,$2,'go') RETURNING id", orgID, t.Name()+"-p").Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func TestRuleServiceCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := metric.NewRuleService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	valid := metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true}
	created, err := svc.Create(ctx, valid)
	if err != nil || created.ID == 0 {
		t.Fatalf("create = (%+v,%v)", created, err)
	}
	// Невалидные.
	for _, bad := range []metric.Rule{
		{ProjectID: projectID, MetricName: "", Aggregation: "avg", Comparator: "gt", WindowSeconds: 300},
		{ProjectID: projectID, MetricName: "m", Aggregation: "bogus", Comparator: "gt", WindowSeconds: 300},
		{ProjectID: projectID, MetricName: "m", Aggregation: "avg", Comparator: "ne", WindowSeconds: 300},
		{ProjectID: projectID, MetricName: "m", Aggregation: "avg", Comparator: "gt", WindowSeconds: 0},
	} {
		if _, err := svc.Create(ctx, bad); !errors.Is(err, metric.ErrInvalidRule) {
			t.Fatalf("Create(%+v) = %v, want ErrInvalidRule", bad, err)
		}
	}
	// List + ListEnabled.
	if rules, _ := svc.List(ctx, projectID); len(rules) != 1 {
		t.Fatalf("List = %d, want 1", len(rules))
	}
	if enabled, _ := svc.ListEnabled(ctx); len(enabled) != 1 {
		t.Fatalf("ListEnabled = %d, want 1", len(enabled))
	}
	// Delete scoped: чужой projectID не удаляет.
	if err := svc.Delete(ctx, created.ID, projectID+999); err != nil {
		t.Fatalf("delete scoped: %v", err)
	}
	if rules, _ := svc.List(ctx, projectID); len(rules) != 1 {
		t.Fatalf("scoped delete removed rule")
	}
	if err := svc.Delete(ctx, created.ID, projectID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rules, _ := svc.List(ctx, projectID); len(rules) != 0 {
		t.Fatalf("rule not deleted")
	}
}

func TestIncidentServiceOpenClose(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	rules := metric.NewRuleService(pool)
	inc := metric.NewIncidentService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rule, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	in, created, err := inc.Open(ctx, rule.ID, projectID, 150)
	if err != nil || !created {
		t.Fatalf("open = (%+v,%v,%v)", in, created, err)
	}
	// Повторный Open → created=false.
	if _, created2, _ := inc.Open(ctx, rule.ID, projectID, 160); created2 {
		t.Fatalf("second open must be created=false")
	}
	// Bump.
	if err := inc.Bump(ctx, in.ID, 160, 160); err != nil {
		t.Fatalf("bump: %v", err)
	}
	// Resolve, повторный → ok=false.
	if ok, _ := inc.Resolve(ctx, in.ID, 50); !ok {
		t.Fatalf("resolve must be ok=true")
	}
	if ok, _ := inc.Resolve(ctx, in.ID, 50); ok {
		t.Fatalf("second resolve must be ok=false")
	}
	// После закрытия новый Open создаёт (created=true).
	if _, created3, _ := inc.Open(ctx, rule.ID, projectID, 200); !created3 {
		t.Fatalf("open after resolve must be created=true")
	}
	// List.
	if list, _ := inc.List(ctx, projectID, 10); len(list) != 2 {
		t.Fatalf("list = %d, want 2", len(list))
	}
}

func TestIncidentOpenConcurrentOnlyOneWins(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	rules := metric.NewRuleService(pool)
	inc := metric.NewIncidentService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rule, _ := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})

	const n = 20
	var wg sync.WaitGroup
	created := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, c, err := inc.Open(ctx, rule.ID, projectID, 100+float64(i))
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
		t.Fatalf("concurrent opens created=true count = %d, want exactly 1", wins)
	}
}
