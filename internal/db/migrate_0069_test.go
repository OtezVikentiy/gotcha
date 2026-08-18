package db_test

// Тест на непустой базе для НОВЕЙШЕЙ миграции живёт в файле этой самой
// новейшей миграции — см. migrate_0070_test.go (0070_org_usage_logs.up.sql,
// C1, задача 2).

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0069HostsAgentVersionAddThenDrop — 0069 добавляет hosts.agent_version
// (TEXT, без NOT NULL/DEFAULT) поверх непустой таблицы hosts, накопленной ещё
// до миграции (0064, T4): ADD COLUMN без DEFAULT — безопасная операция на
// непустой таблице (NULL для существующих строк), но тест на непустой базе
// всё равно нужен по правилу — самый дешёвый способ поймать, если кто-то
// впоследствии допишет сюда NOT NULL без DEFAULT и превратит миграцию в
// ломающую. down проверяет, что колонка исчезает, не трогая саму таблицу
// hosts и её строки.
func TestMigrate0069HostsAgentVersionAddThenDrop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 68); err != nil {
		t.Fatalf("migrate to 68: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m69', 'M69', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm69', 'M69') RETURNING id", orgID)
	var hostID int64
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1, 'm69-web') RETURNING id", projectID)

	if err := db.MigratePGTo(dsn, 69); err != nil {
		t.Fatalf("migrate to 69: %v", err)
	}

	// Существующая строка, накопленная ДО миграции, обязана получить NULL, а
	// не упасть на NOT NULL — ровно тот класс ошибки, ради которого правило
	// TestLatestMigrationHasDataTest существует.
	var agentVersion *string
	if err := pool.QueryRow(ctx,
		"SELECT agent_version FROM hosts WHERE id = $1", hostID).Scan(&agentVersion); err != nil {
		t.Fatalf("select agent_version: %v", err)
	}
	if agentVersion != nil {
		t.Fatalf("agent_version = %v, want NULL для строки, заведённой до миграции", *agentVersion)
	}

	if _, err := pool.Exec(ctx,
		"UPDATE hosts SET agent_version = '0.6.0' WHERE id = $1", hostID); err != nil {
		t.Fatalf("update agent_version: %v", err)
	}

	if err := db.MigratePGTo(dsn, 68); err != nil {
		t.Fatalf("migrate down to 68: %v", err)
	}

	var columnExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'hosts' AND column_name = 'agent_version'
		)`).Scan(&columnExists); err != nil {
		t.Fatalf("check agent_version column: %v", err)
	}
	if columnExists {
		t.Fatal("колонка agent_version должна исчезнуть после отката 0069")
	}

	// Сама строка hosts (и таблица целиком) откат не трогает — DROP COLUMN,
	// не DROP TABLE.
	var name string
	if err := pool.QueryRow(ctx, "SELECT name FROM hosts WHERE id = $1", hostID).Scan(&name); err != nil {
		t.Fatalf("host row must survive down-migration: %v", err)
	}
	if name != "m69-web" {
		t.Fatalf("name = %q, want m69-web", name)
	}
}
