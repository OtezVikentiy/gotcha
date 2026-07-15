package profile_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestFlameBuildsTree(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := profile.NewQuery(conn)
	ctx := context.Background()
	ins := func(stack []string, v uint64) {
		if err := conn.Exec(ctx, `INSERT INTO profile_samples
			(project_id,profile_type,service,environment,transaction,platform,ts,stack,value)
			VALUES (5,'cpu','api','','','go',now64(3),?,?)`, stack, v); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	ins([]string{"root", "a", "x"}, 3)
	ins([]string{"root", "a", "y"}, 2)
	ins([]string{"root", "b"}, 5)

	now := time.Now().UTC()
	root, err := q.Flame(ctx, 5, "api", "", "cpu", "", now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Flame: %v", err)
	}
	if root.Value != 10 {
		t.Fatalf("root value = %d, want 10", root.Value)
	}
	child := map[string]*profile.FlameNode{}
	for _, c := range root.Children {
		child[c.Name] = c
	}
	if child["root"] == nil {
		t.Fatalf("missing root frame")
	}
	var a, b *profile.FlameNode
	for _, c := range child["root"].Children {
		if c.Name == "a" {
			a = c
		}
		if c.Name == "b" {
			b = c
		}
	}
	if a == nil || a.Value != 5 || b == nil || b.Value != 5 {
		t.Fatalf("a/b = %+v/%+v", a, b)
	}
	var x, y *profile.FlameNode
	for _, c := range a.Children {
		if c.Name == "x" {
			x = c
		}
		if c.Name == "y" {
			y = c
		}
	}
	if x == nil || x.Value != 3 || y == nil || y.Value != 2 {
		t.Fatalf("x/y = %+v/%+v", x, y)
	}

	// ListServices.
	svcs, err := q.ListServices(ctx, 5, "", now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(svcs) != 1 || svcs[0].Service != "api" || svcs[0].Samples != 10 {
		t.Fatalf("services = %+v", svcs)
	}
}

func TestFlameForTrace(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := profile.NewQuery(conn)
	ctx := context.Background()
	ins := func(traceID string, stack []string, v uint64) {
		if err := conn.Exec(ctx, `INSERT INTO profile_samples
			(project_id,profile_type,service,environment,transaction,platform,ts,stack,value,trace_id)
			VALUES (7,'cpu','api','','','go',now64(3),?,?,?)`, stack, v, traceID); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	ins("T1", []string{"root", "a"}, 3)
	ins("T1", []string{"root", "b"}, 2)
	ins("T2", []string{"root", "c"}, 9)

	// HasProfileForTrace.
	if ok, err := q.HasProfileForTrace(ctx, 7, "T1"); err != nil || !ok {
		t.Fatalf("HasProfileForTrace(T1) = (%v,%v), want (true,nil)", ok, err)
	}
	if ok, _ := q.HasProfileForTrace(ctx, 7, "T3"); ok {
		t.Fatalf("HasProfileForTrace(T3) must be false")
	}
	if ok, _ := q.HasProfileForTrace(ctx, 7, ""); ok {
		t.Fatalf("empty traceID must be false")
	}

	// FlameForTrace изолирует T1 (root.Value=5, без 'c').
	root, err := q.FlameForTrace(ctx, 7, "T1")
	if err != nil {
		t.Fatalf("FlameForTrace: %v", err)
	}
	if root.Value != 5 {
		t.Fatalf("root value = %d, want 5 (T1 only)", root.Value)
	}
	for _, top := range root.Children {
		for _, c := range top.Children {
			if c.Name == "c" {
				t.Fatalf("T2 stack leaked into T1 flame")
			}
		}
	}
}

func TestSelfShareQueries(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := profile.NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC()

	ins := func(fnLeaf string, v uint64, ago time.Duration) {
		if err := conn.Exec(ctx, `INSERT INTO profile_samples
			(project_id,profile_type,service,environment,transaction,platform,ts,stack,value,trace_id)
			VALUES (9,'cpu','api','','','go',?,?,?,'')`,
			now.Add(-ago), []string{"root", fnLeaf}, v); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// Свежее окно (последний час): slow — 60 из 100 (60%).
	ins("slow", 60, 10*time.Minute)
	ins("fast", 40, 10*time.Minute)
	// Вчера: slow — 10 из 100 (10%) → база ~10%.
	ins("slow", 10, 24*time.Hour)
	ins("fast", 90, 24*time.Hour)

	// ServicesWithProfiles.
	sts, err := q.ServicesWithProfiles(ctx, 9, now.Add(-2*time.Hour), now.Add(time.Minute))
	if err != nil || len(sts) != 1 || sts[0].Service != "api" || sts[0].Type != "cpu" {
		t.Fatalf("services = %+v err=%v", sts, err)
	}
	// TopFunctionsBySelfShare (свежее окно) → slow первым.
	top, err := q.TopFunctionsBySelfShare(ctx, 9, "api", "cpu", now.Add(-time.Hour), now.Add(time.Minute), 10)
	if err != nil || len(top) == 0 || top[0] != "slow" {
		t.Fatalf("top = %v err=%v", top, err)
	}
	// RecentFunctionShare slow ≈ 0.6, samples 100.
	share, samples, err := q.RecentFunctionShare(ctx, 9, "api", "cpu", "slow", now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil || samples != 100 || share < 0.55 || share > 0.65 {
		t.Fatalf("recent share=%v samples=%d err=%v", share, samples, err)
	}
	// Baseline slow за 7 дней — медиана дневных долей (60% сегодня, 10% вчера) → 0.1..0.6.
	base, err := q.BaselineFunctionShare(ctx, 9, "api", "cpu", "slow", 7, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if base <= 0 || base > 0.65 {
		t.Fatalf("baseline = %v, want within (0,0.65]", base)
	}
}
