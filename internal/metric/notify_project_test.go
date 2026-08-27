package metric_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// stubProjectNamer — фиксированное имя проекта, без обращения к БД. Общий
// контур (escalation.Dispatch) принимает его через escalation.ProjectNamer
// duck-typing — metric не знает и не должен знать про этот конкретный тип.
type stubProjectNamer struct{ name string }

func (s stubProjectNamer) ProjectName(context.Context, int64) (string, error) { return s.name, nil }

// TestMetricNotifierIncludesProjectNameWhenWired — W3-E требование 4: имя
// проекта в теме/теле/webhook-payload, когда Projects задан. Второй из
// (минимум) двух источников доказывающих реальный вызов общего контура — см.
// TestHostNotifierIncludesProjectNameWhenWired для первого: если сломать
// сборку темы/тела в escalation.Dispatch, падают ОБА теста в РАЗНЫХ пакетах.
func TestMetricNotifierIncludesProjectNameWhenWired(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	n := &metric.MetricNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details:  alert.NewDetailPolicy("", nil, true),
		Projects: stubProjectNamer{name: "Marketing Site"},
	}
	ev := metric.MetricEvent{
		ProjectID: projectID, MetricName: "http.errors", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, Current: 250, Peak: 300, Environment: "prod", Opened: true,
	}
	if err := n.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: jobs=%+v err=%v", jobs, err)
	}
	subject, _ := jobs[0].Payload["subject"].(string)
	body, _ := jobs[0].Payload["body"].(string)
	if !strings.Contains(subject, "Marketing Site") {
		t.Errorf("subject must name the project: %q", subject)
	}
	if !strings.Contains(body, "Marketing Site") {
		t.Errorf("body must name the project: %q", body)
	}
	if jobs[0].Payload["project_name"] != "Marketing Site" {
		t.Errorf("payload project_name = %v, want %q", jobs[0].Payload["project_name"], "Marketing Site")
	}
}
