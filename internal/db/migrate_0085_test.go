package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0085AddsIdempotentLogConstraint — миграция 0085 (W2-C находка
// 3 аудита 2026-08-27) adds a UNIQUE constraint on incident_escalations
// (incident_source, incident_id, channel_id, step) so escalation.LogStep can
// retry safely (ON CONFLICT DO NOTHING) after a crash between logging a step
// and bumping its escalation level. Pre-existing distinct rows (the normal
// case on any real installation — one row per channel per step) must survive
// the migration untouched; a genuine duplicate insert (same 4-tuple) must be
// rejected once the constraint is in place, proving it's actually enforced
// and not just declared.
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

	// Две РАЗНЫЕ строки (разный channel_id) — обычное, ожидаемое содержимое
	// таблицы на любой существующей инсталляции.
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

	// Идемпотентный путь (то, ради чего констрейнт заведён): ON CONFLICT DO
	// NOTHING на дубле — не ошибка, не создаёт вторую строку.
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

	// Констрейнт реально применяется, а не только объявлен: голый дубль
	// (без ON CONFLICT) обязан упасть.
	if _, err := pool.Exec(ctx, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step)
		VALUES ('metric', 9101, 1, 0)`); err == nil {
		t.Fatal("plain duplicate insert succeeded, want a unique-violation error")
	}

	// escalation_step_log_failures — граница попыток на провал LogStep
	// (условие 2 ревью): таблица обязана появиться вместе с констрейнтом.
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

// TestMigrate0085CollapsesExistingDuplicatesBeforeConstraint — условие 1
// ревью: до этой миграции LogStep не был идемпотентным, и находка 3 ровно
// про то, что дубль (одна и та же ступень, залогированная дважды на ретрае)
// мог уже существовать на живой инсталляции. Голый CREATE UNIQUE упал бы на
// такой базе. Миграция обязана СНАЧАЛА схлопнуть дубль (детерминированно —
// оставить строку с минимальным id) и только потом наложить констрейнт.
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

	// Тот самый дубль, находка про который: одна и та же (source, incident,
	// channel, step) записана дважды (напр. ретрай после краха между логом
	// и bump, до фикса) — с разным sent_at, минимальный id первый.
	var firstID, secondID int64
	mustScan(t, pool, &firstID, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', 9202, 5, 1, now() - interval '10 minutes') RETURNING id`)
	mustScan(t, pool, &secondID, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', 9202, 5, 1, now()) RETURNING id`)
	// Несвязанная строка (другой channel_id) — контрольная: не должна
	// задеться дедупликацией.
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
