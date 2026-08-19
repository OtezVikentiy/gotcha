package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0073AddsHostLabels — колонки environment/role добавляются на
// базе, где в hosts уже есть строка без них (DEFAULT пустой строки обязан
// лечь на непустую таблицу); новый host с явными метками сохраняет их; откат
// снимает обе колонки, не задев саму строку hosts.
func TestMigrate0073AddsHostLabels(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 72); err != nil {
		t.Fatalf("migrate to 72: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m73', 'M73', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm73', 'M73') RETURNING id", orgID)

	// Существующий host до миграции — без environment/role, их ещё нет на v72.
	var hostID int64
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1, 'web-1') RETURNING id", projectID)

	if err := db.MigratePGTo(dsn, 73); err != nil {
		t.Fatalf("migrate to 73: %v", err)
	}

	// DEFAULT пустой строки лёг на непустую таблицу.
	var environment, role string
	if err := pool.QueryRow(ctx,
		"SELECT environment, role FROM hosts WHERE id = $1", hostID).
		Scan(&environment, &role); err != nil {
		t.Fatalf("select existing host: %v", err)
	}
	if environment != "" || role != "" {
		t.Fatalf("existing host environment/role = (%q, %q), want (\"\", \"\")", environment, role)
	}

	// Новый host с явными метками сохраняет их.
	var newHostID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO hosts (project_id, name, environment, role) VALUES ($1, 'web-2', 'production', 'gateway') RETURNING id",
		projectID).Scan(&newHostID); err != nil {
		t.Fatalf("insert host после миграции: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT environment, role FROM hosts WHERE id = $1", newHostID).
		Scan(&environment, &role); err != nil {
		t.Fatalf("select new host: %v", err)
	}
	if environment != "production" || role != "gateway" {
		t.Fatalf("new host environment/role = (%q, %q), want (production, gateway)", environment, role)
	}

	// Откат — колонки исчезают, строка hosts уцелевает.
	if err := db.MigratePGTo(dsn, 72); err != nil {
		t.Fatalf("migrate down to 72: %v", err)
	}
	for _, column := range []string{"environment", "role"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'hosts' AND column_name = $1)`, column).
			Scan(&exists); err != nil {
			t.Fatalf("check column %s: %v", column, err)
		}
		if exists {
			t.Fatalf("колонка hosts.%s должна исчезнуть после отката 0073", column)
		}
	}
	var name string
	if err := pool.QueryRow(ctx, "SELECT name FROM hosts WHERE id = $1", hostID).Scan(&name); err != nil {
		t.Fatalf("host row must survive down-migration: %v", err)
	}
	if name != "web-1" {
		t.Fatalf("host name = %q, want web-1", name)
	}
}
