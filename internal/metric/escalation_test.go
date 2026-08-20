package metric_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestIncidentServiceEscalationName проверяет, что ключ источника совпадает
// с incident_source='metric', зафиксированным в миграции 0077.
func TestIncidentServiceEscalationName(t *testing.T) {
	pool := testenv.MigratedPG(t)
	inc := metric.NewIncidentService(pool)
	if got := inc.Name(); got != "metric" {
		t.Fatalf("Name() = %q, want %q", got, "metric")
	}
}

// TestIncidentServiceOpenUnacked дискриминирует «status='open' AND
// acknowledged_at IS NULL»: открытый неподтверждённый инцидент попадает в
// выборку с верными полями; после Acknowledge — пропадает; отдельный
// resolved-инцидент в выборку не попадает вовсе.
func TestIncidentServiceOpenUnacked(t *testing.T) {
	pool := testenv.MigratedPG(t)
	rules := metric.NewRuleService(pool)
	inc := metric.NewIncidentService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	rule1, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create rule1: %v", err)
	}
	rule2, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "mem", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create rule2: %v", err)
	}

	in, _, err := inc.Open(ctx, rule1.ID, projectID, 150, false)
	if err != nil {
		t.Fatalf("open rule1: %v", err)
	}

	list, err := inc.OpenUnacked(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("OpenUnacked = %d/%v, want 1", len(list), err)
	}
	got := list[0]
	if got.ID != in.ID || got.ProjectID != projectID {
		t.Fatalf("OpenUnacked[0] = %+v, want ID=%d ProjectID=%d", got, in.ID, projectID)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("OpenUnacked[0].StartedAt пуст")
	}
	if got.Severity != "warning" {
		t.Fatalf("OpenUnacked[0].Severity = %q, want %q (metric-дефолт из 0077)", got.Severity, "warning")
	}
	if got.EscalationLevel != 0 {
		t.Fatalf("OpenUnacked[0].EscalationLevel = %d, want 0", got.EscalationLevel)
	}

	in2, _, err := inc.Open(ctx, rule2.ID, projectID, 95, false)
	if err != nil {
		t.Fatalf("open rule2: %v", err)
	}
	if list, err = inc.OpenUnacked(ctx); err != nil || len(list) != 2 {
		t.Fatalf("OpenUnacked после второго open = %d/%v, want 2", len(list), err)
	}
	if ok, err := inc.Resolve(ctx, in2.ID, 10); err != nil || !ok {
		t.Fatalf("Resolve(rule2): (%v,%v), want (true,nil)", ok, err)
	}
	if list, err = inc.OpenUnacked(ctx); err != nil || len(list) != 1 || list[0].ID != in.ID {
		t.Fatalf("OpenUnacked после Resolve = %+v/%v, want только rule1-инцидент", list, err)
	}

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "metric-esc-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if ok, err := inc.Acknowledge(ctx, in.ID, userID); err != nil || !ok {
		t.Fatalf("Acknowledge: (%v,%v), want (true,nil)", ok, err)
	}
	if list, err = inc.OpenUnacked(ctx); err != nil || len(list) != 0 {
		t.Fatalf("OpenUnacked после Acknowledge = %d/%v, want 0", len(list), err)
	}
}

// TestIncidentServiceBumpEscalation проверяет атомарность продвижения
// escalation_level: успешный бамп двигает level и last_escalated_at,
// повторный бамп с устаревшим from — идемпотентный no-op (ok=false).
func TestIncidentServiceBumpEscalation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	rules := metric.NewRuleService(pool)
	inc := metric.NewIncidentService(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	rule, err := rules.Create(ctx, metric.Rule{ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 90, WindowSeconds: 300, Enabled: true})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	in, _, err := inc.Open(ctx, rule.ID, projectID, 150, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	ok, err := inc.BumpEscalation(ctx, in.ID, 0)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(0) = (%v,%v), want (true,nil)", ok, err)
	}
	var level int
	var lastEscalatedAt any
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level, last_escalated_at FROM metric_incidents WHERE id = $1", in.ID).
		Scan(&level, &lastEscalatedAt); err != nil {
		t.Fatalf("select after bump: %v", err)
	}
	if level != 1 {
		t.Fatalf("escalation_level после BumpEscalation(0) = %d, want 1", level)
	}
	if lastEscalatedAt == nil {
		t.Fatal("last_escalated_at после BumpEscalation(0) = nil, want заполнено")
	}

	ok, err = inc.BumpEscalation(ctx, in.ID, 0)
	if err != nil || ok {
		t.Fatalf("повторный BumpEscalation(0) = (%v,%v), want (false,nil)", ok, err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM metric_incidents WHERE id = $1", in.ID).Scan(&level); err != nil {
		t.Fatalf("select after stale bump: %v", err)
	}
	if level != 1 {
		t.Fatalf("escalation_level после устаревшего BumpEscalation = %d, want 1 (не сдвинулся)", level)
	}

	ok, err = inc.BumpEscalation(ctx, in.ID, 1)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(1) = (%v,%v), want (true,nil)", ok, err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM metric_incidents WHERE id = $1", in.ID).Scan(&level); err != nil {
		t.Fatalf("select after second bump: %v", err)
	}
	if level != 2 {
		t.Fatalf("escalation_level после BumpEscalation(1) = %d, want 2", level)
	}
}
