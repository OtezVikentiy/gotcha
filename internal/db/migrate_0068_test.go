package db_test

// Тест на непустой базе для 0068 (host_incidents_host_id_idx.up.sql, T10).
// Новейшая миграция сдвинулась на 0069 (hosts_agent_version, T8, см.
// migrate_0069_test.go) — комментарий-указатель для TestLatestMigrationHasDataTest
// (internal/guards) переехал туда. Тест 0065 (host_threshold_settings, T9)
// трогать не нужно, он остаётся за 0065.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0068HostIncidentsCreateOpenResolveThenDropCascade — миграции
// 0066-0068 (§4.3 дизайна) на непустой базе: 0066 заводит host_incidents с
// project_id/host_id FK, CHECK'ами на kind/status И частичным уникальным
// индексом (host_id, kind) WHERE status='open' — тем самым, на который
// опирается гонко-безопасный IncidentService.Open (второй INSERT того же
// открытого (host_id, kind) обязан упасть на конфликт); 0067/0068 — индексы
// для листинга по проекту и покрытия FK по host_id. Откат до 63 убирает
// таблицу целиком (DROP TABLE каскадом снимает и её индексы).
func TestMigrate0068HostIncidentsCreateOpenResolveThenDropCascade(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 63); err != nil {
		t.Fatalf("migrate to 63: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m68', 'M68', 0) RETURNING id")
	var projectID int64
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm68', 'M68') RETURNING id", orgID)

	if err := db.MigratePGTo(dsn, 68); err != nil {
		t.Fatalf("migrate to 68: %v", err)
	}

	// hosts появилась в 0064 — заводим строку, на которую сошлётся FK host_id.
	var hostID int64
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1, 'm68-web') RETURNING id", projectID)

	// Открываем инцидент — обязательные NOT NULL/CHECK колонки (kind, status)
	// принимают допустимые значения.
	var incidentID int64
	mustScan(t, pool, &incidentID, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1, $2, 'disk', 'open', 0.95, 0.95, '/var full')
		RETURNING id`, projectID, hostID)

	// CHECK на kind отклоняет значение вне закрытого перечня 0066.
	if _, err := pool.Exec(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value)
		VALUES ($1, $2, 'bogus', 'open', 1, 1)`, projectID, hostID); err == nil {
		t.Fatal("insert с kind='bogus' должен упасть на CHECK, но прошёл")
	}

	// 0066: частичный уникальный индекс (host_id, kind) WHERE status='open' —
	// второй открытый инцидент того же (host_id, kind) обязан конфликтовать
	// (реальный сценарий IncidentService.Open под конкурентными вызовами).
	if _, err := pool.Exec(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value)
		VALUES ($1, $2, 'disk', 'open', 0.9, 0.9)`, projectID, hostID); err == nil {
		t.Fatal("второй открытый инцидент того же (host_id, kind) должен упасть на уникальный индекс, но прошёл")
	}

	// Разного kind того же хоста индекс не блокирует.
	if _, err := pool.Exec(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value)
		VALUES ($1, $2, 'load', 'open', 3.0, 3.0)`, projectID, hostID); err != nil {
		t.Fatalf("insert другого kind того же хоста не должен конфликтовать: %v", err)
	}

	// Резолвим первый — после этого повторное открытие того же (host_id,
	// kind='disk') снова проходит: частичный индекс больше не видит строку.
	if _, err := pool.Exec(ctx,
		"UPDATE host_incidents SET status = 'resolved', resolved_at = now() WHERE id = $1", incidentID); err != nil {
		t.Fatalf("resolve incident: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value)
		VALUES ($1, $2, 'disk', 'open', 0.8, 0.8)`, projectID, hostID); err != nil {
		t.Fatalf("insert disk после resolve не должен конфликтовать: %v", err)
	}

	// 0067/0068: индексы существуют (используются планировщиком для
	// листинга по проекту и покрытия FK host_id).
	for _, idx := range []string{
		"host_incidents_one_open_idx",
		"host_incidents_project_started_idx",
		"host_incidents_host_id_idx",
	} {
		var exists bool
		if err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)", idx).Scan(&exists); err != nil {
			t.Fatalf("check index %s: %v", idx, err)
		}
		if !exists {
			t.Fatalf("индекс %s не найден после миграции до 68", idx)
		}
	}

	if err := db.MigratePGTo(dsn, 63); err != nil {
		t.Fatalf("migrate down to 63: %v", err)
	}

	var tableExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'host_incidents')`).
		Scan(&tableExists); err != nil {
		t.Fatalf("check host_incidents table: %v", err)
	}
	if tableExists {
		t.Fatal("таблица host_incidents должна исчезнуть после отката до 63")
	}
}
