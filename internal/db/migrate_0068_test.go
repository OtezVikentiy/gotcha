package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// 0066 заводит host_incidents с частичным уникальным индексом (host_id, kind) WHERE status='open' —
// на нём держится гонко-безопасный IncidentService.Open; 0067/0068 — индексы для листинга/FK.
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

	// Второй открытый инцидент того же (host_id, kind) обязан конфликтовать — сценарий IncidentService.Open.
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

	// После resolve частичный индекс не видит строку — то же (host_id, kind) открывается снова.
	if _, err := pool.Exec(ctx,
		"UPDATE host_incidents SET status = 'resolved', resolved_at = now() WHERE id = $1", incidentID); err != nil {
		t.Fatalf("resolve incident: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value)
		VALUES ($1, $2, 'disk', 'open', 0.8, 0.8)`, projectID, hostID); err != nil {
		t.Fatalf("insert disk после resolve не должен конфликтовать: %v", err)
	}

	// Индексы 0067/0068 — для листинга по проекту и покрытия FK host_id.
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
