package host_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestOpenUnackedPurgedGroupTreatedAsClosed — группа, чья строка физически
// удалена из incident_groups (janitor purge/ретеншен), должна трактоваться
// как закрытая: висячий group_id не блокирует бывшего члена в OpenUnacked
// навсегда (R2a/W3, регресс мутации, убравшей `g.id IS NULL` из предиката —
// прогон полного тестового набора её не ловил). Дополнительно: StartedAt —
// собственный started_at инцидента, а не время резолва группы (группы уже
// нет — COALESCE(g.resolved_at, ...) не с чем сравнивать, GREATEST должен
// схлопнуться в i.started_at).
func TestOpenUnackedPurgedGroupTreatedAsClosed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	rootH := seedEvalHost(t, pool, pid, "gw-01")
	memberH := seedEvalHost(t, pool, pid, "web-01")

	svc := host.NewIncidentService(pool)
	member, _, err := svc.Open(ctx, pid, memberH.ID, "disk", 0.95, "", false)
	if err != nil {
		t.Fatalf("Open member: %v", err)
	}

	rootInc := seedOpenSilentIncident(t, pool, pid, rootH.ID, true)
	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, pid, "host", rootInc, "host", rootH.ID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := store.SetGroup(ctx, pid, "host", member.ID, grp.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	// Симуляция purge: строка группы удалена, но group_id члена (висячий)
	// остаётся — ровно та ситуация, из-за которой W3 в аудите не всплыла ни
	// на одном прогоне пакета (мутация убрала g.id IS NULL из предиката).
	if _, err := pool.Exec(ctx, "DELETE FROM incident_groups WHERE id = $1", grp.ID); err != nil {
		t.Fatalf("delete group (purge): %v", err)
	}

	list, err := svc.OpenUnacked(ctx)
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
