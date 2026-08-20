package recipes_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
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

func TestApplyRulesIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	svc := metric.NewRuleService(pool)
	rec, ok := recipes.ByID("redis")
	if !ok {
		t.Fatal("recipe redis not found")
	}
	if len(rec.Rules) == 0 {
		t.Fatal("redis recipe has no rules")
	}

	created, skipped, err := recipes.ApplyRules(ctx, svc, pid, rec)
	if err != nil || created != len(rec.Rules) || skipped != 0 {
		t.Fatalf("first apply = (%d,%d,%v), want (%d,0,nil)", created, skipped, err, len(rec.Rules))
	}
	rules, err := svc.List(ctx, pid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rules) != len(rec.Rules) {
		t.Fatalf("rules in db = %d, want %d", len(rules), len(rec.Rules))
	}
	wantSeverity := map[string]string{}
	for _, spec := range rec.Rules {
		wantSeverity[spec.Metric] = spec.Severity
	}
	for _, r := range rules {
		if r.Environment != "" || !r.Enabled || r.ProjectID != pid {
			t.Fatalf("created rule %+v: want all-env, enabled, project %d", r, pid)
		}
		if r.Severity != wantSeverity[r.MetricName] {
			t.Fatalf("created rule %s severity = %q, want %q (из RuleSpec)", r.MetricName, r.Severity, wantSeverity[r.MetricName])
		}
	}

	// Second apply: everything already exists.
	created, skipped, err = recipes.ApplyRules(ctx, svc, pid, rec)
	if err != nil || created != 0 || skipped != len(rec.Rules) {
		t.Fatalf("second apply = (%d,%d,%v), want (0,%d,nil)", created, skipped, err, len(rec.Rules))
	}

	// User tuned a threshold: not overwritten, not duplicated (threshold is
	// deliberately NOT part of the idempotency key).
	first := rec.Rules[0]
	if _, err := pool.Exec(ctx,
		"UPDATE metric_alert_rules SET threshold = threshold + 123 WHERE project_id = $1 AND metric_name = $2",
		pid, first.Metric); err != nil {
		t.Fatalf("tune threshold: %v", err)
	}
	created, skipped, err = recipes.ApplyRules(ctx, svc, pid, rec)
	if err != nil || created != 0 || skipped != len(rec.Rules) {
		t.Fatalf("apply after tune = (%d,%d,%v), want (0,%d,nil)", created, skipped, err, len(rec.Rules))
	}
	var tuned float64
	if err := pool.QueryRow(ctx,
		"SELECT threshold FROM metric_alert_rules WHERE project_id = $1 AND metric_name = $2",
		pid, first.Metric).Scan(&tuned); err != nil {
		t.Fatalf("read tuned threshold (dup created?): %v", err)
	}
	if tuned != first.Threshold+123 {
		t.Fatalf("tuned threshold = %v, want %v (must not be overwritten)", tuned, first.Threshold+123)
	}

	// Env-scoped user rule does NOT block the all-env default: drop the
	// all-env rule for the second spec, recreate it env-scoped, re-apply.
	second := rec.Rules[1]
	if _, err := pool.Exec(ctx,
		"DELETE FROM metric_alert_rules WHERE project_id = $1 AND metric_name = $2",
		pid, second.Metric); err != nil {
		t.Fatalf("delete all-env rule: %v", err)
	}
	if _, err := svc.Create(ctx, metric.Rule{
		ProjectID: pid, MetricName: second.Metric, Aggregation: second.Agg,
		Comparator: second.Comparator, Threshold: second.Threshold,
		WindowSeconds: second.WindowSeconds, LabelKey: second.LabelKey,
		LabelValue: second.LabelValue, Environment: "staging", Enabled: true,
	}); err != nil {
		t.Fatalf("create env-scoped rule: %v", err)
	}
	created, skipped, err = recipes.ApplyRules(ctx, svc, pid, rec)
	if err != nil || created != 1 || skipped != len(rec.Rules)-1 {
		t.Fatalf("apply with env-scoped rule = (%d,%d,%v), want (1,%d,nil)", created, skipped, err, len(rec.Rules)-1)
	}
	var allEnvCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM metric_alert_rules WHERE project_id = $1 AND metric_name = $2 AND environment IS NULL",
		pid, second.Metric).Scan(&allEnvCount); err != nil {
		t.Fatalf("count all-env: %v", err)
	}
	if allEnvCount != 1 {
		t.Fatalf("all-env rules for %s = %d, want 1", second.Metric, allEnvCount)
	}
}

func TestRuleStatuses(t *testing.T) {
	rec := recipes.Recipe{ID: "unit", Rules: []recipes.RuleSpec{
		{Metric: "m.a", Agg: "avg", Comparator: "gt", Threshold: 10, WindowSeconds: 300},
		{Metric: "m.b", Agg: "sum", Comparator: "gt", Threshold: 0, WindowSeconds: 300},
		{Metric: "m.c", Agg: "avg", Comparator: "lt", Threshold: 1, WindowSeconds: 600,
			LabelKey: "state", LabelValue: "active"},
	}}
	existing := []metric.Rule{
		// Matches m.a: same key, different threshold (threshold not in key).
		{MetricName: "m.a", Aggregation: "avg", Comparator: "gt", Threshold: 99, WindowSeconds: 60},
		// Same key as m.b but env-scoped: must NOT count as existing.
		{MetricName: "m.b", Aggregation: "sum", Comparator: "gt", Environment: "staging"},
		// Same metric as m.c but different label value: no match.
		{MetricName: "m.c", Aggregation: "avg", Comparator: "lt", LabelKey: "state", LabelValue: "waiting"},
	}
	got := recipes.RuleStatuses(existing, rec)
	if len(got) != len(rec.Rules) {
		t.Fatalf("len = %d, want %d", len(got), len(rec.Rules))
	}
	want := []bool{true, false, false}
	for i, st := range got {
		if st.Spec.Metric != rec.Rules[i].Metric {
			t.Fatalf("status[%d].Spec.Metric = %s, want %s", i, st.Spec.Metric, rec.Rules[i].Metric)
		}
		if st.Exists != want[i] {
			t.Fatalf("status[%d].Exists = %v, want %v", i, st.Exists, want[i])
		}
	}

	// Full-key match including labels.
	existing = append(existing, metric.Rule{
		MetricName: "m.c", Aggregation: "avg", Comparator: "lt",
		LabelKey: "state", LabelValue: "active",
	})
	got = recipes.RuleStatuses(existing, rec)
	if !got[2].Exists {
		t.Fatal("status[2].Exists = false after full-key match, want true")
	}
}
