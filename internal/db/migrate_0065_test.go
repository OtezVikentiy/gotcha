package db_test

// TestLatestMigrationHasDataTest (internal/guards) требует, чтобы НОВЕЙШАЯ
// миграция PostgreSQL приезжала с тестом на непустой базе — db.MigratePGTo на
// схему, уже содержащую строки. На момент этой правки новейшая —
// 0065_host_threshold_settings.up.sql (внутри internal/host — T4 завёл
// 0064-тест 0064, его трогать не нужно, он остаётся за 0064 и после).

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0065HostThresholdSettingsCreateThenDrop — 0065 добавляет
// host_threshold_settings (пороги встроенных инцидентов хоста, §4.2
// дизайна). Проверяем: FK на projects(id) принимает существующий проект,
// PK(project_id) отклоняет вторую строку того же проекта (реальный
// upsert-сценарий SettingsService.Save — INSERT ... ON CONFLICT (project_id)
// DO UPDATE), CHECK'и на диапазоны реально работают на непустой базе, а down
// убирает таблицу целиком.
func TestMigrate0065HostThresholdSettingsCreateThenDrop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 64); err != nil {
		t.Fatalf("migrate to 64: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m65', 'M65', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm65', 'M65') RETURNING id", orgID)

	if err := db.MigratePGTo(dsn, 65); err != nil {
		t.Fatalf("migrate to 65: %v", err)
	}

	// Дефолты применяются, если вставить только project_id.
	var diskThreshold float64
	var silentSeconds int
	if err := pool.QueryRow(ctx,
		`INSERT INTO host_threshold_settings (project_id) VALUES ($1) RETURNING disk_threshold`, projectID).
		Scan(&diskThreshold); err != nil {
		t.Fatalf("insert default settings row: %v", err)
	}
	if diskThreshold != 0.90 {
		t.Fatalf("disk_threshold по умолчанию = %v, want 0.90", diskThreshold)
	}
	if err := pool.QueryRow(ctx,
		"SELECT silent_after_seconds FROM host_threshold_settings WHERE project_id = $1", projectID).
		Scan(&silentSeconds); err != nil {
		t.Fatalf("select silent_after_seconds: %v", err)
	}
	if silentSeconds != 300 {
		t.Fatalf("silent_after_seconds по умолчанию = %d, want 300", silentSeconds)
	}

	// PK(project_id) — тот же upsert-сценарий, каким его использует
	// host.SettingsService.Save (см. internal/host/settings.go).
	if _, err := pool.Exec(ctx,
		`INSERT INTO host_threshold_settings (project_id, disk_threshold) VALUES ($1, 0.5)
		 ON CONFLICT (project_id) DO UPDATE SET disk_threshold = 0.5`, projectID); err != nil {
		t.Fatalf("upsert existing settings row: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT disk_threshold FROM host_threshold_settings WHERE project_id = $1", projectID).
		Scan(&diskThreshold); err != nil {
		t.Fatalf("select disk_threshold after upsert: %v", err)
	}
	if diskThreshold != 0.5 {
		t.Fatalf("disk_threshold после upsert = %v, want 0.5 (PK должен апдейтить ту же строку)", diskThreshold)
	}

	// CHECK-границы проверяем на ОТДЕЛЬНЫХ проектах (не переиспользуем
	// projectID: там уже есть строка, и без ON CONFLICT это дало бы PK-
	// нарушение, а не именно CHECK, который здесь и нужен проверить).
	var checkProjectID int64
	mustScan(t, pool, &checkProjectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm65-check', 'M65 Check') RETURNING id", orgID)

	if _, err := pool.Exec(ctx,
		`INSERT INTO host_threshold_settings (project_id, silent_after_seconds) VALUES ($1, 120)`,
		checkProjectID); err == nil {
		t.Fatal("insert с silent_after_seconds=120 должен упасть на CHECK, но прошёл")
	}
	// disk_threshold вне (0, 1] обязан отклоняться.
	if _, err := pool.Exec(ctx,
		`INSERT INTO host_threshold_settings (project_id, disk_threshold) VALUES ($1, 1.5)`,
		checkProjectID); err == nil {
		t.Fatal("insert с disk_threshold=1.5 должен упасть на CHECK, но прошёл")
	}
	// load_threshold = 0 обязан отклоняться (строго > 0).
	if _, err := pool.Exec(ctx,
		`INSERT INTO host_threshold_settings (project_id, load_threshold) VALUES ($1, 0)`,
		checkProjectID); err == nil {
		t.Fatal("insert с load_threshold=0 должен упасть на CHECK, но прошёл")
	}

	if err := db.MigratePGTo(dsn, 64); err != nil {
		t.Fatalf("migrate down to 64: %v", err)
	}

	var tableExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'host_threshold_settings')`).
		Scan(&tableExists); err != nil {
		t.Fatalf("check host_threshold_settings table: %v", err)
	}
	if tableExists {
		t.Fatal("таблица host_threshold_settings должна исчезнуть после отката 0065")
	}
}
