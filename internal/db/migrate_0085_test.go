package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// UNIQUE(incident_source,incident_id,channel_id,step) даёт LogStep безопасный ретрай (ON CONFLICT DO
// NOTHING) после краха между логом шага и bump уровня; проверяем и что старые строки не пострадали.
func TestMigrate0085AddsIdempotentLogConstraint(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 84); err != nil {
		t.Fatalf("migrate to 84: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	// Обычное, ожидаемое содержимое таблицы (разный channel_id) на любой существующей инсталляции.
	mustExec(t, pool, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step)
		VALUES ('metric', 9101, 1, 0)`)
	mustExec(t, pool, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step)
		VALUES ('metric', 9101, 2, 0)`)

	if err := db.MigratePGTo(dsn, 85); err != nil {
		t.Fatalf("migrate to 85: %v", err)
	}

	if !constraintExists(t, pool, "incident_escalations_source_incident_channel_step_key") {
		t.Fatal("constraint incident_escalations_source_incident_channel_step_key not found after migrating to 85")
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=9101").
		Scan(&count); err != nil {
		t.Fatalf("select pre-existing rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("pre-existing distinct rows after migration = %d, want 2 (untouched)", count)
	}

	// Идемпотентный путь, ради которого констрейнт заведён — ON CONFLICT DO NOTHING не создаёт вторую строку.
	if _, err := pool.Exec(ctx, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step)
		VALUES ('metric', 9101, 1, 0)
		ON CONFLICT (incident_source, incident_id, channel_id, step) DO NOTHING`); err != nil {
		t.Fatalf("idempotent re-insert with ON CONFLICT DO NOTHING: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=9101 AND channel_id=1").
		Scan(&count); err != nil {
		t.Fatalf("select after idempotent re-insert: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows for channel 1 after idempotent re-insert = %d, want 1 (no duplicate)", count)
	}

	// Констрейнт реально применяется, не только объявлен — голый дубль обязан упасть.
	if _, err := pool.Exec(ctx, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step)
		VALUES ('metric', 9101, 1, 0)`); err == nil {
		t.Fatal("plain duplicate insert succeeded, want a unique-violation error")
	}

	// escalation_step_log_failures — граница попыток на провал LogStep, появляется вместе с констрейнтом.
	var tableExists bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'escalation_step_log_failures')").
		Scan(&tableExists); err != nil {
		t.Fatalf("check escalation_step_log_failures exists: %v", err)
	}
	if !tableExists {
		t.Fatal("table escalation_step_log_failures not found after migrating to 85")
	}
}

// До миграции LogStep не был идемпотентным — дубль (та же ступень, залогированная дважды на ретрае)
// мог уже существовать; миграция сначала схлопывает дубль (минимальный id), потом накладывает UNIQUE.
func TestMigrate0085CollapsesExistingDuplicatesBeforeConstraint(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 84); err != nil {
		t.Fatalf("migrate to 84: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	// Дубль той же (source,incident,channel,step) с разным sent_at — минимальный id вставлен первым.
	var firstID, secondID int64
	mustScan(t, pool, &firstID, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', 9202, 5, 1, now() - interval '10 minutes') RETURNING id`)
	mustScan(t, pool, &secondID, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', 9202, 5, 1, now()) RETURNING id`)
	// Контрольная строка (другой channel_id) — дедупликация не должна её задеть.
	var untouchedID int64
	mustScan(t, pool, &untouchedID, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', 9202, 6, 1, now()) RETURNING id`)

	if err := db.MigratePGTo(dsn, 85); err != nil {
		t.Fatalf("migrate to 85 (must survive pre-existing duplicate): %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='host' AND incident_id=9202 AND channel_id=5 AND step=1").
		Scan(&count); err != nil {
		t.Fatalf("select after dedup: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows for the duplicated 4-tuple after migration = %d, want 1 (collapsed)", count)
	}

	var survivingID int64
	if err := pool.QueryRow(ctx,
		"SELECT id FROM incident_escalations WHERE incident_source='host' AND incident_id=9202 AND channel_id=5 AND step=1").
		Scan(&survivingID); err != nil {
		t.Fatalf("select surviving row: %v", err)
	}
	if survivingID != firstID {
		t.Errorf("surviving row id = %d, want %d (deterministic: minimum id kept, %d discarded)", survivingID, firstID, secondID)
	}

	var untouchedCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE id = $1", untouchedID).Scan(&untouchedCount); err != nil {
		t.Fatalf("select untouched row: %v", err)
	}
	if untouchedCount != 1 {
		t.Errorf("untouched row (different channel) = %d rows, want 1: dedup must not have touched it", untouchedCount)
	}

	if !constraintExists(t, pool, "incident_escalations_source_incident_channel_step_key") {
		t.Fatal("constraint not found after migrating to 85")
	}
}
