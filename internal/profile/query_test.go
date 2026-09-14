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

	svcs, err := q.ListServices(ctx, 5, "", now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(svcs) != 1 || svcs[0].Service != "api" || svcs[0].Weight != 10 || svcs[0].Samples != 3 {
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

	if ok, err := q.HasProfileForTrace(ctx, 7, "T1"); err != nil || !ok {
		t.Fatalf("HasProfileForTrace(T1) = (%v,%v), want (true,nil)", ok, err)
	}
	if ok, _ := q.HasProfileForTrace(ctx, 7, "T3"); ok {
		t.Fatalf("HasProfileForTrace(T3) must be false")
	}
	if ok, _ := q.HasProfileForTrace(ctx, 7, ""); ok {
		t.Fatalf("empty traceID must be false")
	}

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
	ins("slow", 60, 10*time.Minute)
	ins("fast", 40, 10*time.Minute)
	ins("slow", 10, 24*time.Hour)
	ins("fast", 90, 24*time.Hour)

	sts, err := q.ServicesWithProfiles(ctx, 9, now.Add(-2*time.Hour), now.Add(time.Minute))
	if err != nil || len(sts) != 1 || sts[0].Service != "api" || sts[0].Type != "cpu" {
		t.Fatalf("services = %+v err=%v", sts, err)
	}
	top, err := q.TopFunctionsBySelfShare(ctx, 9, "api", "cpu", now.Add(-time.Hour), now.Add(time.Minute), 10)
	if err != nil || len(top) == 0 || top[0] != "slow" {
		t.Fatalf("top = %v err=%v", top, err)
	}
}

func TestBaselineFunctionSharesSamples(t *testing.T) {
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
			VALUES (11,'cpu','api','','','go',?,?,?,'')`,
			now.Add(-ago), []string{"root", fnLeaf}, v); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	ins("slow", 20, 10*time.Minute)
	ins("slow", 20, 10*time.Minute)
	ins("slow", 20, 10*time.Minute)
	ins("fast", 40, 10*time.Minute)
	ins("slow", 5, 24*time.Hour)
	ins("slow", 5, 24*time.Hour)
	ins("fast", 90, 24*time.Hour)

	base, err := q.BaselineFunctionShares(ctx, 11, "api", "cpu", []string{"slow", "missing"}, 7, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	slow, ok := base["slow"]
	if !ok {
		t.Fatalf("no baseline for slow: %+v", base)
	}
	if slow.Samples != 5 {
		t.Fatalf("slow.Samples = %d, want 5 (число строк функции за окно, не вес и не итог окна из 7 строк)", slow.Samples)
	}
	if slow.Share < 0.1 || slow.Share > 0.6 {
		t.Fatalf("slow.Share = %v, want within [0.1,0.6]", slow.Share)
	}
	if _, ok := base["missing"]; ok {
		t.Fatalf("baseline has key for a function absent from the window: %+v", base)
	}
	if _, ok := base["fast"]; ok {
		t.Fatalf("fast вне списка не должна попадать в выдачу: %+v", base)
	}

	base, err = q.BaselineFunctionShares(ctx, 11, "api", "cpu", []string{"fast"}, 7, now.Add(time.Minute))
	if err != nil || base["fast"].Samples != 2 {
		t.Fatalf("fast = %+v err=%v, want Samples=2", base["fast"], err)
	}

	base, err = q.BaselineFunctionShares(ctx, 11, "api", "cpu", nil, 7, now.Add(time.Minute))
	if err != nil || len(base) != 0 {
		t.Fatalf("empty list: %+v err=%v, want empty", base, err)
	}
}

func TestRegressionGateOnRowCountNotWeight(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := profile.NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC()
	cfg := profile.DefaultProfileRegressionConfig()

	insertRows := func(projectID uint64, fn string, value uint64, ts time.Time, n int) {
		batch, err := conn.PrepareBatch(ctx, `INSERT INTO profile_samples (
			project_id, profile_type, service, environment, transaction, platform, ts, stack, value, unit, trace_id)`)
		if err != nil {
			t.Fatalf("prepare batch: %v", err)
		}
		for i := 0; i < n; i++ {
			if err := batch.Append(projectID, "cpu", "api", "", "", "go", ts,
				[]string{"root", fn}, value, "nanoseconds", ""); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		if err := batch.Send(); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	const thin = 21
	insertRows(thin, "hot", 1_000_000_000, now.Add(-24*time.Hour), 150)
	insertRows(thin, "filler", 9_000_000_000, now.Add(-24*time.Hour), 150)
	insertRows(thin, "hot", 1_000_000_000, now.Add(-48*time.Hour), 150)
	insertRows(thin, "filler", 9_000_000_000, now.Add(-48*time.Hour), 150)
	insertRows(thin, "hot", 1_000_000_000, now.Add(-10*time.Minute), 3)

	recentFrom, recentTo := now.Add(-time.Hour), now.Add(time.Minute)
	shares, err := q.TopFunctionShares(ctx, thin, "api", "cpu", recentFrom, recentTo, 10)
	if err != nil {
		t.Fatalf("TopFunctionShares: %v", err)
	}
	if len(shares) != 1 || shares[0].Function != "hot" {
		t.Fatalf("shares = %+v, want single 'hot'", shares)
	}
	if shares[0].Samples != 3 {
		t.Fatalf("thin window Samples = %d, want 3 (число строк, не вес 3_000_000_000)", shares[0].Samples)
	}

	baselines, err := q.BaselineFunctionShares(ctx, thin, "api", "cpu", []string{"hot"}, 7, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BaselineFunctionShares: %v", err)
	}
	base := baselines["hot"]
	if base.Samples < uint64(cfg.MinSamples) {
		t.Fatalf("base.Samples = %d, want >= MinSamples so the gate under test is the recent window's", base.Samples)
	}

	if got := profile.Decide(base.Share, shares[0].Share, base.Samples, shares[0].Samples, cfg, false).Kind; got != profile.DecisionNone {
		t.Fatalf("Decide on 3-row window = %v, want DecisionNone (MinSamples must gate on row count)", got)
	}

	const full = 22
	insertRows(full, "hot", 1_000_000_000, now.Add(-24*time.Hour), 150)
	insertRows(full, "filler", 9_000_000_000, now.Add(-24*time.Hour), 150)
	insertRows(full, "hot", 1_000_000_000, now.Add(-48*time.Hour), 150)
	insertRows(full, "filler", 9_000_000_000, now.Add(-48*time.Hour), 150)
	insertRows(full, "hot", 1_000_000_000, now.Add(-10*time.Minute), 150)

	shares, err = q.TopFunctionShares(ctx, full, "api", "cpu", recentFrom, recentTo, 10)
	if err != nil {
		t.Fatalf("TopFunctionShares (full): %v", err)
	}
	if len(shares) != 1 || shares[0].Function != "hot" {
		t.Fatalf("shares (full) = %+v, want single 'hot'", shares)
	}
	if shares[0].Samples != 150 {
		t.Fatalf("full window Samples = %d, want 150", shares[0].Samples)
	}

	baselines, err = q.BaselineFunctionShares(ctx, full, "api", "cpu", []string{"hot"}, 7, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BaselineFunctionShares (full): %v", err)
	}
	base = baselines["hot"]

	if got := profile.Decide(base.Share, shares[0].Share, base.Samples, shares[0].Samples, cfg, false).Kind; got != profile.DecisionOpen {
		t.Fatalf("Decide on 150-row window = %v, want DecisionOpen (enough samples, share far above base)", got)
	}
}

func TestTopFunctionSharesShareByWeightSamplesByCount(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := profile.NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC()

	ins := func(fnLeaf string, v uint64, n int) {
		for i := 0; i < n; i++ {
			if err := conn.Exec(ctx, `INSERT INTO profile_samples
				(project_id,profile_type,service,environment,transaction,platform,ts,stack,value,trace_id)
				VALUES (31,'cpu','api','','','go',?,?,?,'')`,
				now.Add(-10*time.Minute), []string{"root", fnLeaf}, v); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	ins("a", 100, 2)
	ins("b", 1, 8)

	shares, err := q.TopFunctionShares(ctx, 31, "api", "cpu", now.Add(-time.Hour), now.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("TopFunctionShares: %v", err)
	}
	if len(shares) != 2 {
		t.Fatalf("shares = %+v, want 2 functions", shares)
	}
	byFn := map[string]profile.FunctionShare{}
	for _, sh := range shares {
		byFn[sh.Function] = sh
	}
	a, ok := byFn["a"]
	if !ok {
		t.Fatalf("no share for 'a': %+v", shares)
	}
	b, ok := byFn["b"]
	if !ok {
		t.Fatalf("no share for 'b': %+v", shares)
	}

	if a.Share < 0.9 || a.Share > 1.0 {
		t.Fatalf("a.Share = %v, want ~0.9615 (self/total по весу, не 2/10 по числу строк)", a.Share)
	}
	if b.Share < 0.0 || b.Share > 0.1 {
		t.Fatalf("b.Share = %v, want ~0.0385 (self/total по весу, не 8/10 по числу строк)", b.Share)
	}

	if a.Samples != 2 {
		t.Fatalf("a.Samples = %d, want 2 (число строк САМОЙ 'a', не всего окна (10) и не вес)", a.Samples)
	}
	if b.Samples != 8 {
		t.Fatalf("b.Samples = %d, want 8 (число строк САМОЙ 'b', не всего окна (10) и не вес)", b.Samples)
	}
}

func TestTopFunctionSharesEmptyFunctionExcludedButWeighsIn(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := profile.NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC()
	ts := now.Add(-10 * time.Minute)

	insStack := func(stack []string, v uint64, n int) {
		for i := 0; i < n; i++ {
			if err := conn.Exec(ctx, `INSERT INTO profile_samples
				(project_id,profile_type,service,environment,transaction,platform,ts,stack,value,trace_id)
				VALUES (32,'cpu','api','','','go',?,?,?,'')`,
				ts, stack, v); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	insStack([]string{"root", "a"}, 100, 2)
	insStack([]string{"root", "b"}, 1, 8)
	insStack([]string{}, 100, 5)

	shares, err := q.TopFunctionShares(ctx, 32, "api", "cpu", now.Add(-time.Hour), now.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("TopFunctionShares: %v", err)
	}
	byFn := map[string]profile.FunctionShare{}
	for _, sh := range shares {
		byFn[sh.Function] = sh
	}
	if _, ok := byFn[""]; ok {
		t.Fatalf("empty-name group leaked into output: %+v", shares)
	}
	if len(shares) != 2 {
		t.Fatalf("shares = %+v, want exactly 2 (a, b), no empty-name entry", shares)
	}
	a, ok := byFn["a"]
	if !ok {
		t.Fatalf("no share for 'a': %+v", shares)
	}
	b, ok := byFn["b"]
	if !ok {
		t.Fatalf("no share for 'b': %+v", shares)
	}

	if a.Share < 0.27 || a.Share > 0.30 {
		t.Fatalf("a.Share = %v, want ~0.2825 (self/(named+unnamed)=200/708, не 200/208)", a.Share)
	}
	if b.Share < 0.008 || b.Share > 0.02 {
		t.Fatalf("b.Share = %v, want ~0.0113 (self/(named+unnamed)=8/708, не 8/208)", b.Share)
	}
	if a.Samples != 2 || b.Samples != 8 {
		t.Fatalf("Samples = a:%d b:%d, want a=2 b=8 (строки САМОЙ функции; безымянные весят в total, но не считаются как чьи-то строки)", a.Samples, b.Samples)
	}
}
