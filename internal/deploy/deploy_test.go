package deploy_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// setupProject поднимает мигрированную PG-базу и одну организацию/проект —
// заготовка, общая для тестов пакета (калька host/store_test.go setupProject).
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

// TestDeployStore — сквозной путь стора: Record возвращает id/created_at, List
// отдаёт окно newest-first, Nearest находит ближайший предшествующий деплой и
// сообщает ok=false до всех деплоев, Recent чужого проекта пуст (тенант-изоляция).
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

	// List окна newest-first
	list, err := st.List(ctx, pid, t0.Add(-3*time.Hour), t0, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Version != "v1.1.0" {
		t.Fatalf("List = %+v, want 2 newest-first", list)
	}

	// Recent — тоже newest-first
	recent, err := st.Recent(ctx, pid, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 2 || recent[0].Version != "v1.1.0" {
		t.Fatalf("Recent = %+v, want 2 newest-first", recent)
	}

	// Nearest ДО момента
	near, ok, err := st.Nearest(ctx, pid, t0.Add(-15*time.Minute))
	if err != nil || !ok || near.Version != "v1.1.0" {
		t.Fatalf("Nearest = %+v ok=%v err=%v, want v1.1.0", near, ok, err)
	}
	// Nearest без предшествующего — ok=false
	_, ok2, err := st.Nearest(ctx, pid, t0.Add(-3*time.Hour))
	if err != nil {
		t.Fatalf("Nearest до всех деплоев: err=%v", err)
	}
	if ok2 {
		t.Fatalf("Nearest до всех деплоев должен вернуть ok=false")
	}

	// тенант-изоляция: чужой проект пуст
	other, err := st.Recent(ctx, int64(999), 10)
	if err != nil {
		t.Fatalf("Recent чужого проекта: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("чужой проект не пуст: %+v", other)
	}
}
