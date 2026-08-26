package slo_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestSLOOpenUnackedPurgedGroupTreatedAsClosed — зеркало
// host/group_purged_gate_test.go на slo_incidents (R2a/W3, регресс мутации,
// убравшей `g.id IS NULL` из предиката OpenUnacked — прогон полного
// тестового набора её не ловил). Группа, чья строка физически удалена из
// incident_groups (janitor purge/ретеншен), должна трактоваться как
// закрытая: висячий group_id не блокирует бывшего члена в OpenUnacked
// навсегда. StartedAt — собственный started_at инцидента (группы уже нет,
// GREATEST не должен подмешивать время резолва).
func TestSLOOpenUnackedPurgedGroupTreatedAsClosed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	monID := seedGroupMonitor(t, pool, pid)
	rootInc := seedOpenUptimeIncident(t, pool, monID, true)

	st := slo.NewStore(pool)
	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "api uptime", Kind: slo.SLIUptime, MonitorID: &monID,
		Target: 0.99, WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	member, created, err := st.OpenIncident(ctx, s.ID, pid, 20.0, nil, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident: created=%v err=%v", created, err)
	}

	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, pid, "uptime", rootInc, "monitor", monID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := store.SetGroup(ctx, pid, "slo", member.ID, grp.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	// Симуляция purge: строка группы удалена, group_id члена остаётся
	// висячим.
	if _, err := pool.Exec(ctx, "DELETE FROM incident_groups WHERE id = $1", grp.ID); err != nil {
		t.Fatalf("delete group (purge): %v", err)
	}

	list, err := st.OpenUnacked(ctx)
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
