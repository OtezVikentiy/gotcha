package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0087FillsExistingNullsBeforeNotNull — миграция на непустой
// базе: строка host_incidents с NULL в current_value/peak_value (след
// ручной вставки мимо IncidentService, единственного продового писателя,
// который такого NULL не оставляет) должна пережить накат 0087 с
// current_value/peak_value, обнулёнными вместо ошибки ALTER COLUMN ...
// SET NOT NULL на непустой колонке с NULL-строками.
func TestMigrate0087FillsExistingNullsBeforeNotNull(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 86); err != nil {
		t.Fatalf("migrate to 86: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, hostID, incidentID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m87', 'M87', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm87', 'M87') RETURNING id", orgID)
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1, 'web-01') RETURNING id", projectID)
	// Строка с NULL в обеих колонках — до 0087 схема это разрешает, любой
	// путь записи Go его не оставляет, но ручной SQL мог.
	mustScan(t, pool, &incidentID,
		`INSERT INTO host_incidents (project_id, host_id, kind, status, current_value, peak_value)
		 VALUES ($1, $2, 'disk', 'open', NULL, NULL) RETURNING id`, projectID, hostID)

	if err := db.MigratePGTo(dsn, 87); err != nil {
		t.Fatalf("migrate to 87: %v", err)
	}

	var current, peak float64
	if err := pool.QueryRow(ctx,
		"SELECT current_value, peak_value FROM host_incidents WHERE id = $1", incidentID).
		Scan(&current, &peak); err != nil {
		t.Fatalf("select после 0087: %v", err)
	}
	if current != 0 || peak != 0 {
		t.Fatalf("current_value=%v peak_value=%v, want 0/0 (NULL-страховка миграции 0087 не сработала)", current, peak)
	}

	// Ограничение реально установлено: вставка NULL теперь отвергается БД.
	_, err = pool.Exec(ctx,
		`INSERT INTO host_incidents (project_id, host_id, kind, status, current_value, peak_value)
		 VALUES ($1, $2, 'memory', 'open', NULL, 1)`, projectID, hostID)
	if err == nil {
		t.Fatal("вставка NULL в current_value после 0087 прошла без ошибки, want NOT NULL violation")
	}

	_, err = pool.Exec(ctx,
		`INSERT INTO host_incidents (project_id, host_id, kind, status, current_value, peak_value)
		 VALUES ($1, $2, 'load', 'open', 1, NULL)`, projectID, hostID)
	if err == nil {
		t.Fatal("вставка NULL в peak_value после 0087 прошла без ошибки, want NOT NULL violation")
	}
}

// TestMigrate0087DownRestoresNullable — откат снимает NOT NULL: down должен
// применяться без ошибки, а вставка NULL снова проходить.
func TestMigrate0087DownRestoresNullable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 87); err != nil {
		t.Fatalf("migrate to 87: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, hostID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m87d', 'M87D', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm87d', 'M87D') RETURNING id", orgID)
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1, 'web-01') RETURNING id", projectID)

	if err := db.MigratePGTo(dsn, 86); err != nil {
		t.Fatalf("migrate down to 86: %v", err)
	}

	mustExec(t, pool,
		`INSERT INTO host_incidents (project_id, host_id, kind, status, current_value, peak_value)
		 VALUES ($1, $2, 'silent', 'open', NULL, NULL)`, projectID, hostID)
}
