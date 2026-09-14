package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Нужны и пул, и DSN одной базы — testenv.PostgresDSN каждый раз создаёт новую.
func migratedWithDSN(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("MigratePG: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, dsn
}

// Изображает откат релиза: схема ушла вперёд, бинарь — назад.
func forceSchemaVersion(t *testing.T, pool *pgxpool.Pool, version int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE schema_migrations SET version = $1", version); err != nil {
		t.Fatalf("force schema version %d: %v", version, err)
	}
}

func declareCompat(t *testing.T, pool *pgxpool.Pool, target string, version int64, compatible bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO schema_compat (target, version, backward_compatible) VALUES ($1,$2,$3)
		 ON CONFLICT (target, version) DO UPDATE SET backward_compatible = EXCLUDED.backward_compatible`,
		target, version, compatible); err != nil {
		t.Fatalf("declare compat %s/%d: %v", target, version, err)
	}
}

func currentSchemaVersion(t *testing.T, dsn string) int64 {
	t.Helper()
	v, dirty, err := db.SchemaVersion(dsn)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if dirty {
		t.Fatalf("SchemaVersion dirty после миграции")
	}
	return int64(v)
}

func TestRecordSchemaCompatWritesBothSchemas(t *testing.T) {
	pool, dsn := migratedWithDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.RecordSchemaCompat(ctx, pool); err != nil {
		t.Fatalf("RecordSchemaCompat: %v", err)
	}
	if err := db.RecordSchemaCompat(ctx, pool); err != nil {
		t.Fatalf("RecordSchemaCompat повторно: %v", err)
	}

	embedded, err := db.EmbeddedCompatPG()
	if err != nil {
		t.Fatalf("EmbeddedCompatPG: %v", err)
	}
	applied := currentSchemaVersion(t, dsn)

	for version, compatible := range embedded {
		var stored bool
		err := pool.QueryRow(ctx,
			"SELECT backward_compatible FROM schema_compat WHERE target='pg' AND version=$1",
			int64(version)).Scan(&stored)
		if err != nil {
			t.Errorf("признак PG-версии %d не записан: %v", version, err)
			continue
		}
		if stored != compatible {
			t.Errorf("PG-версия %d: в базе %v, в файле %v", version, stored, compatible)
		}
	}
	if int64(len(embedded)) != applied {
		t.Errorf("записано признаков %d при применённой версии %d — часть миграций осталась без признака",
			len(embedded), applied)
	}

	var chCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_compat WHERE target='ch'").Scan(&chCount); err != nil {
		t.Fatalf("count ch: %v", err)
	}
	if chCount == 0 {
		t.Errorf("признаки CH-схемы не записаны — откат бинаря запретят из-за отсутствия записей")
	}
}

// Без таблицы schema_compat каждая вставка проваливается — сообщение обязано называть
// один и тот же номер версии из раза в раз, а не зависеть от обхода Go-карты.
func TestRecordSchemaCompatFailureNamesVersionDeterministically(t *testing.T) {
	pool, err := db.NewPostgres(context.Background(), testenv.PostgresDSN(t))
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first := db.RecordSchemaCompatPG(ctx, pool)
	second := db.RecordSchemaCompatPG(ctx, pool)
	if first == nil || second == nil {
		t.Fatalf("RecordSchemaCompatPG на немигрированной базе должен провалиться: first=%v second=%v", first, second)
	}
	if first.Error() != second.Error() {
		t.Errorf("сообщение об ошибке меняется между вызовами:\n1: %s\n2: %s", first.Error(), second.Error())
	}
}

func TestSchemaGateAllowsRollbackThroughAdditiveMigration(t *testing.T) {
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

	if err := db.CheckSchemaCurrent(ctx, pool, dsn); err != nil {
		t.Errorf("старт запрещён на схеме, впереди которой только аддитивные миграции: %v", err)
	}
}

func TestSchemaGateRejectsRollbackThroughBreakingMigration(t *testing.T) {
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

	err := db.CheckSchemaCurrent(ctx, pool, dsn)
	if err == nil {
		t.Fatalf("старт разрешён на схеме с обратно-несовместимой миграцией")
	}
	if !strings.Contains(err.Error(), "несовместим") {
		t.Errorf("ошибка %q не называет причину — обратную несовместимость", err)
	}
}

func TestSchemaGateRejectsUnknownAheadVersion(t *testing.T) {
	pool, dsn := migratedWithDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.RecordSchemaCompat(ctx, pool); err != nil {
		t.Fatalf("RecordSchemaCompat: %v", err)
	}
	applied := currentSchemaVersion(t, dsn)
	forceSchemaVersion(t, pool, applied+1) // признак не объявляем

	err := db.CheckSchemaCurrent(ctx, pool, dsn)
	if err == nil {
		t.Fatalf("старт разрешён на схеме версии %d, о совместимости которой ничего не известно", applied+1)
	}
	if !strings.Contains(err.Error(), "нет записи") {
		t.Errorf("ошибка %q не объясняет, что признак совместимости неизвестен", err)
	}
}

func TestSchemaGateStillRejectsLaggingSchema(t *testing.T) {
	pool, dsn := migratedWithDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	applied := currentSchemaVersion(t, dsn)
	forceSchemaVersion(t, pool, applied-1)

	err := db.CheckSchemaCurrent(ctx, pool, dsn)
	if err == nil {
		t.Fatalf("старт разрешён на отставшей схеме — вставки телеметрии падали бы на каждой строке")
	}
	if !strings.Contains(err.Error(), "отстаёт") {
		t.Errorf("ошибка %q не объясняет отставание", err)
	}
}
