package metric_test

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedMetricGauge(t *testing.T, ch driver.Conn, projectID int64, name string, val float64, ago time.Duration) {
	t.Helper()
	if err := ch.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'gauge', '1', 'api', '', map(), ?, ?, 0, [], [], 0, '')`,
		projectID, name, time.Now().UTC().Add(-ago), val); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}
}

func TestEvaluatorOpenCloseAlertOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	projectID := seedProject(t, pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}

	eval := &metric.Evaluator{
		Rules: rules, Query: metric.NewQuery(ch), Incidents: incidents,
		Notifier: &metric.MetricNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example"},
		Interval: time.Hour, // тикер не используем — дёргаем Tick вручную
	}

	// Среднее 150 > 100 → инцидент открыт, одно уведомление.
	seedMetricGauge(t, ch, projectID, "cpu", 140, time.Minute)
	seedMetricGauge(t, ch, projectID, "cpu", 160, 2*time.Minute)
	eval.Tick(ctx)

	if _, open, _ := incidents.OpenFor(ctx, rule.ID); !open {
		t.Fatalf("incident must be open after breach")
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Fatalf("open jobs = %d, want 1", len(jobs))
	}

	// Повторный тик при тех же данных → инцидент открыт, НОВЫХ уведомлений нет.
	eval.Tick(ctx)
	jobs2, _ := ob.Claim(ctx, 10)
	if len(jobs2) != 0 {
		t.Fatalf("re-tick produced %d new jobs, want 0 (alert once)", len(jobs2))
	}

	// Данные упали ниже порога (окно теперь содержит только низкие значения) →
	// инцидент закрыт, одно уведомление о закрытии.
	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedMetricGauge(t, ch, projectID, "cpu", 50, time.Minute)
	eval.Tick(ctx)

	if _, open, _ := incidents.OpenFor(ctx, rule.ID); open {
		t.Fatalf("incident must be resolved after recovery")
	}
	jobs3, _ := ob.Claim(ctx, 10)
	if len(jobs3) != 1 {
		t.Fatalf("close jobs = %d, want 1", len(jobs3))
	}
}
