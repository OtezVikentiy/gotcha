package incidentgroup_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func randSlug(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

func mustScan(t *testing.T, pool *pgxpool.Pool, dst *int64, sql string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("scan %q: %v", sql, err)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedProject — организация + проект.
func seedProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var orgID, projectID int64
	slug := "ig-" + randSlug(t)
	mustScan(t, pool, &orgID,
		`INSERT INTO organizations (slug,name,event_quota) VALUES ($1,$1,0) RETURNING id`, slug)
	mustScan(t, pool, &projectID,
		`INSERT INTO projects (org_id,slug,name) VALUES ($1,$2,$2) RETURNING id`, orgID, slug)
	return projectID
}

func seedHost(t *testing.T, pool *pgxpool.Pool, projectID int64, name string) int64 {
	t.Helper()
	var hostID int64
	mustScan(t, pool, &hostID,
		`INSERT INTO hosts (project_id, name, environment, role) VALUES ($1,$2,'','') RETURNING id`,
		projectID, name)
	return hostID
}

// seedSilent — открытый silent-инцидент хоста; notified управляет гейтом
// «информирующего корня».
func seedSilent(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64, notified bool) int64 {
	t.Helper()
	var id int64
	mustScan(t, pool, &id, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',$3) RETURNING id`, projectID, hostID, notified)
	return id
}

func TestEnsureGroupIdempotentAndResolve(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	hostID := seedHost(t, pool, projectID, "root-"+randSlug(t))
	incID := seedSilent(t, pool, projectID, hostID, true)

	store := incidentgroup.NewStore(pool)
	g1, err := store.EnsureGroup(ctx, projectID, "host", incID, "host", hostID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	g2, err := store.EnsureGroup(ctx, projectID, "host", incID, "host", hostID)
	if err != nil {
		t.Fatalf("EnsureGroup 2nd: %v", err)
	}
	if g1.ID != g2.ID {
		t.Fatalf("EnsureGroup not idempotent: %d != %d", g1.ID, g2.ID)
	}
	ok, err := store.Resolve(ctx, "host", incID)
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	ok, err = store.Resolve(ctx, "host", incID)
	if err != nil || ok {
		t.Fatalf("Resolve second call must be no-op: ok=%v err=%v", ok, err)
	}
}

func TestSetGroupFirstWriteWins(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	hostID := seedHost(t, pool, projectID, "h-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, hostID, true)
	memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)

	store := incidentgroup.NewStore(pool)
	g1, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", hostID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := store.SetGroup(ctx, "host", memberInc, g1.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	// Повторный attach к другой группе — no-op (первый выигрывает).
	if err := store.SetGroup(ctx, "host", memberInc, g1.ID+1000); err != nil {
		t.Fatalf("SetGroup 2nd: %v", err)
	}
	var got int64
	mustScan(t, pool, &got, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc)
	if got != g1.ID {
		t.Fatalf("first attach must win: got group_id=%d want %d", got, g1.ID)
	}
	if err := store.SetGroup(ctx, "trace", memberInc, g1.ID); err == nil {
		t.Fatalf("unknown source must error")
	}
}
