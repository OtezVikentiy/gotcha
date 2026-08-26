package metric_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMetricOpenUnackedPurgedGroupTreatedAsClosed — зеркало
// host/group_purged_gate_test.go на metric_incidents (R2a/W3, регресс
// мутации, убравшей `g.id IS NULL` из предиката OpenUnacked — прогон
// полного тестового набора её не ловил). Группа, чья строка физически
// удалена из incident_groups (janitor purge/ретеншен), должна трактоваться
// как закрытая: висячий group_id не блокирует бывшего члена в OpenUnacked
// навсегда. StartedAt — собственный started_at инцидента (группы уже нет,
// GREATEST не должен подмешивать время резолва).
func TestMetricOpenUnackedPurgedGroupTreatedAsClosed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	rootID := seedGroupHost(t, pool, pid, "gw-01")

	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: pid, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, LabelKey: "host", LabelValue: "web-01", Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	member, created, err := incidents.Open(ctx, rule.ID, pid, 150, false, "")
	if err != nil || !created {
		t.Fatalf("Open member: created=%v err=%v", created, err)
	}

	rootInc := seedGroupSilentIncident(t, pool, pid, rootID, true)
	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, pid, "host", rootInc, "host", rootID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := store.SetGroup(ctx, pid, "metric", member.ID, grp.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	// Симуляция purge: строка группы удалена, group_id члена остаётся
	// висячим.
	if _, err := pool.Exec(ctx, "DELETE FROM incident_groups WHERE id = $1", grp.ID); err != nil {
		t.Fatalf("delete group (purge): %v", err)
	}

	list, err := incidents.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked: %v", err)
	}
	var got *escalation.PendingIncident
	for i := range list {
		if list[i].ID == member.ID {
			p := list[i]
			got = &p
		}
	}
	if got == nil {
		t.Fatalf("член группы, чья строка удалена (purge), отсутствует в OpenUnacked — эскалация зависшего инцидента заблокирована навсегда: %+v", list)
	}
	if !got.StartedAt.Equal(member.StartedAt) {
		t.Errorf("StartedAt = %v, want собственный started_at инцидента %v (группы уже нет, GREATEST не должен подмешивать время резолва)", got.StartedAt, member.StartedAt)
	}
}
