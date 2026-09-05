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

// Репетиция отката бинаря нашла дыру: при включённой автомиграции (дефолт)
// первым шагом идёт db.MigratePG, и golang-migrate падает на схеме впереди
// встроенной раньше, чем управление доходит до пост-миграционного
// CheckSchemaCurrent — сообщением библиотеки, а не подготовленным текстом
// гейта. CheckSchemaAhead/CheckSchemaAheadCH — узкая проверка «только
// опережение», вызываемая ДО миграции, чтобы ловить именно этот случай.

// TestCheckSchemaAheadAllowsLaggingOrEqual — отставание и точное совпадение не
// её работа: это чинит миграция, а не гейт. Вмешательство здесь сломало бы
// обычный сценарий обновления (схема ещё не мигрирована, got < want).
func TestCheckSchemaAheadAllowsLaggingOrEqual(t *testing.T) {
	pool, dsn := migratedWithDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// got == want: сразу после полной миграции.
	if err := db.CheckSchemaAhead(ctx, pool, dsn); err != nil {
		t.Errorf("CheckSchemaAhead на точно применённой схеме: %v", err)
	}

	// got < want: схема ещё не мигрирована на последнюю версию.
	applied := currentSchemaVersion(t, dsn)
	forceSchemaVersion(t, pool, applied-1)
	if err := db.CheckSchemaAhead(ctx, pool, dsn); err != nil {
		t.Errorf("CheckSchemaAhead на отставшей схеме: %v (это работа миграции, не гейта)", err)
	}
}

// TestCheckSchemaAheadAllowsCompatibleAdditive — база на две версии впереди
// бинаря, обе аддитивны: старт разрешён (то же послабление, что и в
// CheckSchemaCurrent).
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

// TestCheckSchemaAheadRejectsBreakingMigration — среди версий впереди есть
// обратно-несовместимая: гейт отказывает, и ошибка называет её номер (иначе
// оператор не поймёт, что именно откатывать/чинить).
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

// TestCheckSchemaAheadRejectsUnknownVersion — версия впереди, признака в
// schema_compat нет: fail-closed, старт запрещён.
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

// chSchemaVersion читает текущую версию CH-схемы и флаг dirty напрямую из
// журнала schema_migrations (последняя запись по sequence — см. TestForceCH
// в migrate_force_test.go, тот же приём).
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

// forceSchemaVersionCH дописывает в CH-журнал schema_migrations строку с
// заданной версией (не dirty) — изображает базу, к которой применили
// миграции, которых в этом бинаре нет. Таблица CH-драйвера golang-migrate —
// журнал: читается последняя запись по sequence (см. TestForceCH).
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

// currentSchemaVersionCH — версия CH-схемы сразу после MigrateCH, dirty=false.
func currentSchemaVersionCH(t *testing.T, ctx context.Context, dsn string) uint {
	t.Helper()
	v, dirty := chSchemaVersion(t, ctx, dsn)
	if dirty {
		t.Fatalf("CH-схема dirty после миграции")
	}
	return v
}

// TestCheckSchemaAheadCHAllowsCompatibleAdditive — CH-аналог
// TestCheckSchemaAheadAllowsCompatibleAdditive: база впереди бинаря на
// аддитивную CH-миграцию — старт разрешён.
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

// TestCheckSchemaAheadCHRejectsUnknownVersion — CH-аналог fail-closed: версия
// впереди, признака нет.
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
