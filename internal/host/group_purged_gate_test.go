package host_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

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
