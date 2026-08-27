package host_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// stubProjectNamer — фиксированное имя проекта, без обращения к БД. Общий
// контур (escalation.Dispatch) принимает его через escalation.ProjectNamer
// duck-typing — host не знает и не должен знать про этот конкретный тип.
type stubProjectNamer struct{ name string }

func (s stubProjectNamer) ProjectName(context.Context, int64) (string, error) { return s.name, nil }

// TestHostNotifierIncludesProjectNameWhenWired — W3-E требование 4: имя
// проекта в теме/теле/webhook-payload, когда Projects задан. Это ОДИН из
// (минимум) двух источников, доказывающих реальный вызов общего контура —
// см. TestMetricNotifierIncludesProjectNameWhenWired для второго.
func TestHostNotifierIncludesProjectNameWhenWired(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "web-01")
	seedNotifyChannel(t, asvc, projectID)

	n := &host.HostNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details:  alert.NewDetailPolicy("", nil, true),
		Projects: stubProjectNamer{name: "Marketing Site"},
	}
	in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "disk", CurrentValue: 0.95, PeakValue: 0.97}
	s := host.Settings{DiskThreshold: 0.90}

	if err := n.HostIncidentOpened(ctx, in, h, s); err != nil {
		t.Fatalf("HostIncidentOpened: %v", err)
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

// TestHostNotifierOmitsProjectNameWithoutProjects — nil-совместимость:
// Projects не задан (как во всех прочих тестах этого файла, написанных до
// W3-E) — subject/body/payload не меняются.
func TestHostNotifierOmitsProjectNameWithoutProjects(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "web-02")
	seedNotifyChannel(t, asvc, projectID)

	n := &host.HostNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true)}
	in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "disk", CurrentValue: 0.95, PeakValue: 0.97}
	s := host.Settings{DiskThreshold: 0.90}

	if err := n.HostIncidentOpened(ctx, in, h, s); err != nil {
		t.Fatalf("HostIncidentOpened: %v", err)
	}
	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: jobs=%+v err=%v", jobs, err)
	}
	if _, ok := jobs[0].Payload["project_name"]; ok {
		t.Errorf("project_name must be absent without Projects, got %v", jobs[0].Payload["project_name"])
	}
}
