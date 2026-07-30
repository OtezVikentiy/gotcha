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

// migratedWithDSN выдаёт уникальную базу с применёнными миграциями и её DSN.
// Отдельно от testenv.MigratedPG, потому что тестам окна совместимости нужен и
// пул, и DSN одной и той же базы: каждый вызов PostgresDSN создаёт новую.
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

// forceSchemaVersion переписывает версию в schema_migrations, изображая базу,
// к которой применили миграции, которых в этом бинаре нет. Так выглядит откат
// релиза: схема ушла вперёд, бинарь вернулся назад.
func forceSchemaVersion(t *testing.T, pool *pgxpool.Pool, version int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE schema_migrations SET version = $1", version); err != nil {
		t.Fatalf("force schema version %d: %v", version, err)
	}
}

// declareCompat вручную объявляет признак версии, которой в этом бинаре нет.
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

// TestRecordSchemaCompatWritesBothSchemas — признаки обеих схем записываются и
// переживают повторный вызов: миграции применяет каждый старт.
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

// TestSchemaGateAllowsRollbackThroughAdditiveMigration — то, ради чего окно
// заводилось: бинарь, не знающий последней применённой миграции, стартует, если
// та аддитивна. Раньше здесь был безусловный отказ, и вернуть прошлый релиз
// можно было только восстановлением базы из бэкапа.
func TestSchemaGateAllowsRollbackThroughAdditiveMigration(t *testing.T) {
	pool, dsn := migratedWithDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.RecordSchemaCompat(ctx, pool); err != nil {
		t.Fatalf("RecordSchemaCompat: %v", err)
	}
	applied := currentSchemaVersion(t, dsn)

	// База ушла на две версии вперёд относительно этого бинаря; обе аддитивны.
	declareCompat(t, pool, "pg", applied+1, true)
	declareCompat(t, pool, "pg", applied+2, true)
	forceSchemaVersion(t, pool, applied+2)

	if err := db.CheckSchemaCurrent(ctx, pool, dsn); err != nil {
		t.Errorf("старт запрещён на схеме, впереди которой только аддитивные миграции: %v", err)
	}
}

// TestSchemaGateRejectsRollbackThroughBreakingMigration — если среди
// недостающих версий есть ломающая, отказ остаётся: стартовать на схеме, где
// нужной колонки уже нет, значит менять внятную ошибку при старте на ошибку в
// каждой вставке телеметрии.
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

// TestSchemaGateRejectsUnknownAheadVersion — версия впереди, признака нет:
// схему применял бинарь, не знавший о признаке, и утверждать о ней нечего.
// Fail-closed.
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

// TestSchemaGateStillRejectsLaggingSchema — послабление касается только одной
// стороны: отставшая схема по-прежнему не даёт стартовать.
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
