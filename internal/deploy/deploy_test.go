package deploy_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func setupProject(t *testing.T) (*deploy.Store, int64) {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('c5-test', 'C5 Test', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'c5-test', 'C5 Test') RETURNING id", orgID).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return deploy.NewStore(pool), projectID
}

func TestDeployStore(t *testing.T) {
	st, pid := setupProject(t)
	ctx := context.Background()

	t0 := time.Now().UTC().Truncate(time.Second)

	d1, err := st.Record(ctx, pid, deploy.Deployment{Version: "v1.0.0", Environment: "prod", DeployedAt: t0.Add(-2 * time.Hour), URL: "https://ci/1"})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if d1.ID == 0 {
		t.Fatalf("Record не вернул id")
	}
	if d1.ProjectID != pid {
		t.Fatalf("Record ProjectID = %d, want %d", d1.ProjectID, pid)
	}
	if d1.CreatedAt.IsZero() {
		t.Fatalf("Record не вернул created_at")
	}
	if _, err := st.Record(ctx, pid, deploy.Deployment{Version: "v1.1.0", Environment: "prod", DeployedAt: t0.Add(-30 * time.Minute)}); err != nil {
		t.Fatalf("Record #2: %v", err)
	}

	list, err := st.List(ctx, pid, t0.Add(-3*time.Hour), t0, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Version != "v1.1.0" {
		t.Fatalf("List = %+v, want 2 newest-first", list)
	}

	recent, err := st.Recent(ctx, pid, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 2 || recent[0].Version != "v1.1.0" {
		t.Fatalf("Recent = %+v, want 2 newest-first", recent)
	}

	near, ok, err := st.Nearest(ctx, pid, t0.Add(-15*time.Minute))
	if err != nil || !ok || near.Version != "v1.1.0" {
		t.Fatalf("Nearest = %+v ok=%v err=%v, want v1.1.0", near, ok, err)
	}
	_, ok2, err := st.Nearest(ctx, pid, t0.Add(-3*time.Hour))
	if err != nil {
		t.Fatalf("Nearest до всех деплоев: err=%v", err)
	}
	if ok2 {
		t.Fatalf("Nearest до всех деплоев должен вернуть ok=false")
	}

	other, err := st.Recent(ctx, int64(999), 10)
	if err != nil {
		t.Fatalf("Recent чужого проекта: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("чужой проект не пуст: %+v", other)
	}
}

func TestDeployTenantIsolationRealProject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('c5-iso', 'C5 Iso', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projA, projB int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'c5-iso-a', 'A') RETURNING id", orgID).Scan(&projA); err != nil {
		t.Fatalf("insert project A: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'c5-iso-b', 'B') RETURNING id", orgID).Scan(&projB); err != nil {
		t.Fatalf("insert project B: %v", err)
	}

	st := deploy.NewStore(pool)
	t0 := time.Now().UTC().Truncate(time.Second)
	if _, err := st.Record(ctx, projA, deploy.Deployment{Version: "vA", Environment: "prod", DeployedAt: t0.Add(-time.Hour)}); err != nil {
		t.Fatalf("record A: %v", err)
	}

	if got, err := st.Recent(ctx, projB, 10); err != nil || len(got) != 0 {
		t.Fatalf("Recent(B) = %+v err=%v, want пусто", got, err)
	}
	if got, err := st.List(ctx, projB, t0.Add(-2*time.Hour), t0, 10); err != nil || len(got) != 0 {
		t.Fatalf("List(B) = %+v err=%v, want пусто", got, err)
	}
	if _, ok, err := st.Nearest(ctx, projB, t0); err != nil || ok {
		t.Fatalf("Nearest(B) ok=%v err=%v, want ok=false (деплой A не течёт в B)", ok, err)
	}
	if got, err := st.Recent(ctx, projA, 10); err != nil || len(got) != 1 {
		t.Fatalf("Recent(A) = %+v err=%v, want 1", got, err)
	}
}

func TestDeployNearestBoundaryEqual(t *testing.T) {
	st, pid := setupProject(t)
	ctx := context.Background()

	at := time.Now().UTC().Truncate(time.Second)
	if _, err := st.Record(ctx, pid, deploy.Deployment{Version: "v-boundary", Environment: "prod", DeployedAt: at}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	near, ok, err := st.Nearest(ctx, pid, at)
	if err != nil || !ok || near.Version != "v-boundary" {
		t.Fatalf("Nearest(==deployed_at) = %+v ok=%v err=%v, want v-boundary", near, ok, err)
	}
	if _, ok2, err := st.Nearest(ctx, pid, at.Add(-time.Second)); err != nil {
		t.Fatalf("Nearest(before-1s): err=%v", err)
	} else if ok2 {
		t.Fatalf("Nearest на секунду раньше деплоя должен дать ok=false")
	}
}
