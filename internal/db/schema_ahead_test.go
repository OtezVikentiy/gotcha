package db_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestCheckSchemaAheadAllowsLaggingOrEqual(t *testing.T) {
	pool, dsn := migratedWithDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.CheckSchemaAhead(ctx, pool, dsn); err != nil {
		t.Errorf("CheckSchemaAhead на точно применённой схеме: %v", err)
	}

	applied := currentSchemaVersion(t, dsn)
	forceSchemaVersion(t, pool, applied-1)
	if err := db.CheckSchemaAhead(ctx, pool, dsn); err != nil {
		t.Errorf("CheckSchemaAhead на отставшей схеме: %v (это работа миграции, не гейта)", err)
	}
}

func TestCheckSchemaAheadAllowsCompatibleAdditive(t *testing.T) {
	pool, dsn := migratedWithDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.RecordSchemaCompat(ctx, pool); err != nil {
		t.Fatalf("RecordSchemaCompat: %v", err)
	}
	applied := currentSchemaVersion(t, dsn)

	declareCompat(t, pool, "pg", applied+1, true)
	declareCompat(t, pool, "pg", applied+2, true)
	forceSchemaVersion(t, pool, applied+2)

	if err := db.CheckSchemaAhead(ctx, pool, dsn); err != nil {
		t.Errorf("старт запрещён на схеме, впереди которой только аддитивные миграции: %v", err)
	}
}

func TestCheckSchemaAheadRejectsBreakingMigration(t *testing.T) {
	pool, dsn := migratedWithDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.RecordSchemaCompat(ctx, pool); err != nil {
		t.Fatalf("RecordSchemaCompat: %v", err)
	}
	applied := currentSchemaVersion(t, dsn)

	declareCompat(t, pool, "pg", applied+1, true)
	declareCompat(t, pool, "pg", applied+2, false) // например, DROP COLUMN
	forceSchemaVersion(t, pool, applied+2)

	err := db.CheckSchemaAhead(ctx, pool, dsn)
	if err == nil {
		t.Fatalf("старт разрешён на схеме с обратно-несовместимой миграцией")
	}
	if !strings.Contains(err.Error(), "несовместим") {
		t.Errorf("ошибка %q не называет причину — обратную несовместимость", err)
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(applied+2, 10)) {
		t.Errorf("ошибка %q не называет номер несовместимой версии %d", err, applied+2)
	}
}

func TestCheckSchemaAheadRejectsUnknownVersion(t *testing.T) {
	pool, dsn := migratedWithDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.RecordSchemaCompat(ctx, pool); err != nil {
		t.Fatalf("RecordSchemaCompat: %v", err)
	}
	applied := currentSchemaVersion(t, dsn)
	forceSchemaVersion(t, pool, applied+1) // признак не объявляем

	err := db.CheckSchemaAhead(ctx, pool, dsn)
	if err == nil {
		t.Fatalf("старт разрешён на схеме версии %d, о совместимости которой ничего не известно", applied+1)
	}
	if !strings.Contains(err.Error(), "нет записи") {
		t.Errorf("ошибка %q не объясняет, что признак совместимости неизвестен", err)
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(applied+1, 10)) {
		t.Errorf("ошибка %q не называет номер неизвестной версии %d", err, applied+1)
	}
}

// golang-migrate у CH хранит schema_migrations как append-only журнал —
// текущая версия и dirty — последняя запись по sequence, не единственная строка.
func chSchemaVersion(t *testing.T, ctx context.Context, dsn string) (version uint, dirty bool) {
	t.Helper()
	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	defer conn.Close()
	var v int64
	var d uint8
	if err := conn.QueryRow(ctx,
		"SELECT version, dirty FROM schema_migrations ORDER BY sequence DESC LIMIT 1").
		Scan(&v, &d); err != nil {
		t.Fatalf("read ch version: %v", err)
	}
	return uint(v), d != 0
}

func forceSchemaVersionCH(t *testing.T, ctx context.Context, dsn string, version uint) {
	t.Helper()
	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	defer conn.Close()
	if err := conn.Exec(ctx,
		"INSERT INTO schema_migrations (version, dirty, sequence) VALUES (?, 0, ?)",
		int64(version), uint64(time.Now().UnixNano())); err != nil {
		t.Fatalf("force ch schema version %d: %v", version, err)
	}
}

func currentSchemaVersionCH(t *testing.T, ctx context.Context, dsn string) uint {
	t.Helper()
	v, dirty := chSchemaVersion(t, ctx, dsn)
	if dirty {
		t.Fatalf("CH-схема dirty после миграции")
	}
	return v
}

func TestCheckSchemaAheadCHAllowsCompatibleAdditive(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	dsn := testenv.ClickHouseDSN(t)
	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("MigrateCH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := testenv.MigratedPG(t)
	if err := db.RecordSchemaCompatCH(ctx, pool); err != nil {
		t.Fatalf("RecordSchemaCompatCH: %v", err)
	}
	applied := currentSchemaVersionCH(t, ctx, dsn)

	declareCompat(t, pool, "ch", int64(applied)+1, true)
	forceSchemaVersionCH(t, ctx, dsn, applied+1)

	if err := db.CheckSchemaAheadCH(ctx, pool, dsn); err != nil {
		t.Errorf("старт запрещён на CH-схеме, впереди которой только аддитивная миграция: %v", err)
	}
}

func TestCheckSchemaAheadCHRejectsUnknownVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	dsn := testenv.ClickHouseDSN(t)
	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("MigrateCH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := testenv.MigratedPG(t)
	if err := db.RecordSchemaCompatCH(ctx, pool); err != nil {
		t.Fatalf("RecordSchemaCompatCH: %v", err)
	}
	applied := currentSchemaVersionCH(t, ctx, dsn)
	forceSchemaVersionCH(t, ctx, dsn, applied+1) // признак не объявляем

	err := db.CheckSchemaAheadCH(ctx, pool, dsn)
	if err == nil {
		t.Fatalf("старт разрешён на CH-схеме версии %d, о совместимости которой ничего не известно", applied+1)
	}
	if !strings.Contains(err.Error(), "нет записи") {
		t.Errorf("ошибка %q не объясняет, что признак совместимости неизвестен", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(int(applied+1))) {
		t.Errorf("ошибка %q не называет номер неизвестной версии %d", err, applied+1)
	}
}
