package slo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('slo-test', 'SLO Test', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'slo-test', 'SLO Test') RETURNING id", orgID).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

func TestSLOStore(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	in := slo.SLO{ProjectID: pid, Name: "checkout p95", Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 30, Transaction: "POST /checkout", BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true}
	got, err := st.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID == 0 {
		t.Fatalf("Create не вернул id")
	}

	list, err := st.List(ctx, pid)
	if err != nil || len(list) != 1 || list[0].Name != "checkout p95" {
		t.Fatalf("List = %+v err=%v", list, err)
	}

	enabled, err := st.ListEnabled(ctx)
	if err != nil || len(enabled) != 1 || enabled[0].ID != got.ID {
		t.Fatalf("ListEnabled = %+v err=%v", enabled, err)
	}

	one, ok, err := st.Get(ctx, pid, got.ID)
	if err != nil || !ok || one.Target != 0.99 {
		t.Fatalf("Get = %+v ok=%v err=%v", one, ok, err)
	}
	_, ok2, _ := st.Get(ctx, int64(999999), got.ID)
	if ok2 {
		t.Fatalf("чужой проект не должен видеть SLO")
	}

	rem := 0.5
	inc, created, err := st.OpenIncident(ctx, got.ID, pid, 20.0, &rem, false)
	if err != nil || !created || inc.Status != "open" {
		t.Fatalf("OpenIncident = %+v created=%v err=%v", inc, created, err)
	}
	_, created2, err := st.OpenIncident(ctx, got.ID, pid, 25.0, &rem, false)
	if err != nil || created2 {
		t.Fatalf("второй open не должен создавать (one-open): created=%v err=%v", created2, err)
	}

	incs, err := st.Incidents(ctx, pid, got.ID, 10)
	if err != nil || len(incs) != 1 || incs[0].Status != "open" {
		t.Fatalf("Incidents = %+v err=%v", incs, err)
	}

	if err := st.MarkNotified(ctx, inc.ID, true); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}

	resolvedInc, resolved, err := st.ResolveIncident(ctx, got.ID)
	if err != nil || !resolved || resolvedInc.Status != "resolved" {
		t.Fatalf("ResolveIncident = %+v resolved=%v err=%v", resolvedInc, resolved, err)
	}
	_, resolved2, err := st.ResolveIncident(ctx, got.ID)
	if err != nil || resolved2 {
		t.Fatalf("повторный ResolveIncident resolved=%v err=%v", resolved2, err)
	}

	_, created3, err := st.OpenIncident(ctx, got.ID, pid, 30.0, nil, false)
	if err != nil || !created3 {
		t.Fatalf("после resolve open должен создавать: created=%v err=%v", created3, err)
	}

	if err := st.Delete(ctx, pid, got.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list2, _ := st.List(ctx, pid)
	if len(list2) != 0 {
		t.Fatalf("после Delete список не пуст: %+v", list2)
	}

	for i := 0; i < 100; i++ {
		if _, err := st.Create(ctx, slo.SLO{
			ProjectID: pid, Name: "cap", Kind: slo.SLIAvailability, Target: 0.99,
			WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
		}); err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
	}
	_, err = st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "over", Kind: slo.SLIAvailability, Target: 0.99,
		WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if !errors.Is(err, slo.ErrTooManySLOs) {
		t.Fatalf("101-й SLO: err = %v, want ErrTooManySLOs", err)
	}
	capped, err := st.List(ctx, pid)
	if err != nil || len(capped) != 100 {
		t.Fatalf("List после капа = %d err=%v, want 100", len(capped), err)
	}
}

func TestSLOStoreAcknowledge(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "slo-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	def, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "ack slo", Kind: slo.SLIAvailability, Target: 0.99,
		WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rem := 0.5
	inc, created, err := st.OpenIncident(ctx, def.ID, pid, 20.0, &rem, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident = (%+v,%v,%v)", inc, created, err)
	}

	list, err := st.Incidents(ctx, pid, def.ID, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil || list[0].AcknowledgedBy != nil {
		t.Fatalf("до Acknowledge: list=%+v err=%v, want AcknowledgedAt/By nil", list, err)
	}

	ok, err := st.Acknowledge(ctx, inc.ID, pid, userID)
	if err != nil || !ok {
		t.Fatalf("Acknowledge = (%v,%v), want (true,nil)", ok, err)
	}

	list, err = st.Incidents(ctx, pid, def.ID, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt == nil {
		t.Fatalf("после Acknowledge: list=%+v err=%v, want AcknowledgedAt заполнено", list, err)
	}
	if list[0].AcknowledgedBy == nil || *list[0].AcknowledgedBy != userID {
		t.Fatalf("после Acknowledge: AcknowledgedBy = %v, want %d", list[0].AcknowledgedBy, userID)
	}

	if ok2, err := st.Acknowledge(ctx, inc.ID, pid, userID); err != nil || ok2 {
		t.Fatalf("повторный Acknowledge = (%v,%v), want (false,nil)", ok2, err)
	}

	if _, resolved, err := st.ResolveIncident(ctx, def.ID); err != nil || !resolved {
		t.Fatalf("ResolveIncident = (%v,%v)", resolved, err)
	}
	if okClosed, err := st.Acknowledge(ctx, inc.ID, pid, userID); err != nil || okClosed {
		t.Fatalf("Acknowledge закрытого = (%v,%v), want (false,nil)", okClosed, err)
	}
}

func TestSLOStoreAcknowledgeForeignProject(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	// вторым seedProject(t, pool) нельзя: он заводит org с фиксированным slug
	// 'slo-test', повтор упёрся бы в organizations_slug_key.
	var otherPID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) SELECT org_id, 'slo-test-2', 'SLO Test 2' FROM projects WHERE id = $1 RETURNING id",
		pid).Scan(&otherPID); err != nil {
		t.Fatalf("insert other project: %v", err)
	}
	st := slo.NewStore(pool)

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "slo-ack-foreign@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	def, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "ack slo foreign", Kind: slo.SLIAvailability, Target: 0.99,
		WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rem := 0.5
	inc, created, err := st.OpenIncident(ctx, def.ID, pid, 20.0, &rem, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident = (%+v,%v,%v)", inc, created, err)
	}

	if ok, err := st.Acknowledge(ctx, inc.ID, otherPID, userID); err != nil || ok {
		t.Fatalf("Acknowledge с чужим project_id = (%v,%v), want (false,nil)", ok, err)
	}

	list, err := st.Incidents(ctx, pid, def.ID, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil {
		t.Fatalf("после чужого Acknowledge: list=%+v err=%v, want AcknowledgedAt nil", list, err)
	}
}

// Delete должен отличать «удалил» от «удалять было нечего» — иначе web-слой
// отдаёт успех по устаревшему id, а запись остаётся на месте.
func TestSLOStoreDeleteNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	def, err := st.Create(ctx, slo.SLO{ProjectID: pid, Name: "to delete", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, Transaction: "GET /", BurnThreshold: 14.4, BurnLongMin: 60,
		BurnShortMin: 5, Enabled: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := st.Delete(ctx, pid, def.ID+100500); !errors.Is(err, slo.ErrNotFound) {
		t.Fatalf("Delete несуществующего = %v, want ErrNotFound", err)
	}

	if err := st.Delete(ctx, int64(999999), def.ID); !errors.Is(err, slo.ErrNotFound) {
		t.Fatalf("Delete из чужого проекта = %v, want ErrNotFound", err)
	}
	if list, err := st.List(ctx, pid); err != nil || len(list) != 1 {
		t.Fatalf("после чужого Delete list=%+v err=%v, want 1 запись", list, err)
	}

	if err := st.Delete(ctx, pid, def.ID); err != nil {
		t.Fatalf("Delete своего: %v", err)
	}
	if list, err := st.List(ctx, pid); err != nil || len(list) != 0 {
		t.Fatalf("после Delete list=%+v err=%v, want пусто", list, err)
	}
	if err := st.Delete(ctx, pid, def.ID); !errors.Is(err, slo.ErrNotFound) {
		t.Fatalf("повторный Delete = %v, want ErrNotFound", err)
	}
}
