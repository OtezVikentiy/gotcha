package slo_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// erroringSLOGroupHook — фейковая реализация sloGroupHook (duck-typing D3,
// см. evaluator.go): Attach всегда возвращает заданную ошибку. Изолирует
// groupGate от настоящего incidentgroup.Grouper (используется для
// позитивных сценариев в group_test.go).
type erroringSLOGroupHook struct {
	err error
}

func (h *erroringSLOGroupHook) Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (bool, bool, error) {
	return false, false, h.err
}

// TestGroupGateAttachErrorStaysNoisy — groupGate: ошибка Attach — fail-safe
// (докблок groupGate: «шумим как без D3»). uptime-SLO с привязанным
// монитором обязан открыться и уведомить, как будто группы нет вовсе.
func TestGroupGateAttachErrorStaysNoisy(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pid := seedProject(t, pool)
	if _, err := alert.NewService(pool).CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	monID := seedGroupMonitor(t, pool, pid)
	seedOpenUptimeIncident(t, pool, monID, true)

	st := slo.NewStore(pool)
	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "api uptime attach-err", Kind: slo.SLIUptime, MonitorID: &monID,
		Target: 0.99, WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	notifier := &capturingNotifier{store: st}
	e := &slo.Evaluator{
		Pool:           pool,
		Store:          st,
		Providers:      map[slo.SLIKind]slo.Provider{slo.SLIUptime: burningProvider{}},
		Notifier:       notifier,
		Policy:         escalation.NewPolicyStore(pool),
		IncidentGroups: &erroringSLOGroupHook{err: errors.New("attach boom")},
	}

	if _, err := e.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	inc, open, err := st.OpenIncidentFor(ctx, s.ID)
	if err != nil || !open {
		t.Fatalf("OpenIncidentFor: open=%v err=%v", open, err)
	}
	if readSLOGroupID(t, pool, inc.ID) != nil {
		t.Error("Attach error must leave the incident ungrouped")
	}
	if evs := notifier.snapshot(); len(evs) != 1 {
		t.Errorf("Attach error must not suppress the open notification (fail-noisy): events = %d, want 1: %+v", len(evs), evs)
	}
	if !strings.Contains(buf.String(), "group attach failed") {
		t.Errorf("Attach error must be logged, got: %s", buf.String())
	}
}
