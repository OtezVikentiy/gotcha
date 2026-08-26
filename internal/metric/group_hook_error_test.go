package metric_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// erroringMetricGroupHook — фейковая реализация metricGroupHook (duck-
// typing D3, см. evaluator.go): AttachMetric всегда возвращает заданную
// ошибку. Изолирует groupGate от настоящего incidentgroup.Grouper
// (используется для позитивных сценариев в group_test.go).
type erroringMetricGroupHook struct {
	err error
}

func (h *erroringMetricGroupHook) AttachMetric(ctx context.Context, incidentID, projectID int64, hostName string) (bool, bool, error) {
	return false, false, h.err
}

// TestGroupGateAttachMetricErrorStaysNoisy — groupGate: ошибка AttachMetric
// — fail-safe (докблок groupGate: «шумим как без D3»). Инцидент правила
// label_key='host' обязан открыться и уведомить, как будто группы нет
// вовсе, а не молча потеряться под гейтом «attached && informing».
func TestGroupGateAttachMetricErrorStaysNoisy(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pid := seedProject(t, pool)
	if _, err := alert.NewService(pool).CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}

	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: pid, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, LabelKey: "host", LabelValue: "gate-err-host", Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	seedHostLabeledGauge(t, ch, pid, "cpu", "gate-err-host", 150, time.Minute)

	ob := notify.NewOutbox(pool)
	eval := newGroupEvaluator(pool, ch, rules, incidents, ob)
	eval.IncidentGroups = &erroringMetricGroupHook{err: errors.New("attach metric boom")}

	eval.Tick(ctx)

	in, open, err := incidents.OpenFor(ctx, rule.ID)
	if err != nil || !open {
		t.Fatalf("OpenFor: open=%v err=%v", open, err)
	}
	if readMetricGroupID(t, pool, in.ID) != nil {
		t.Error("AttachMetric error must leave the incident ungrouped")
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Errorf("AttachMetric error must not suppress the open notification: jobs = %d, want 1", len(jobs))
	}
	if !strings.Contains(buf.String(), "group attach failed") {
		t.Errorf("AttachMetric error must be logged, got: %s", buf.String())
	}
}
