package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/clickhouse" // driver: clickhouse://
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"     // driver: pgx5://
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	_ "github.com/ClickHouse/clickhouse-go/v2" // регистрирует database/sql драйвер "clickhouse"
)

//go:embed migrations/pg/*.sql
var pgMigrations embed.FS

//go:embed migrations/ch/*.sql
var chMigrations embed.FS

// MigratePG применяет PG-миграции. Идемпотентна.
func MigratePG(dsn string) error {
	return up("migrations/pg", pgMigrations, pgx5URL(dsn))
}

// pgx5URL rewrites a postgres DSN to the URL scheme registered by
// golang-migrate's pgx/v5 driver.
func pgx5URL(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgres://")
	case strings.HasPrefix(dsn, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgresql://")
	}
	return dsn
}

// MigrateCH применяет CH-миграции. Идемпотентна.
func MigrateCH(dsn string) error {
	return up("migrations/ch", chMigrations, dsn)
}

func up(dir string, fsys embed.FS, url string) error {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return fmt.Errorf("migrations source %s: %w", dir, err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return fmt.Errorf("migrate init %s: %w", dir, err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up %s: %w", dir, err)
	}
	return nil
}

// ApplyRetention выставляет TTL таблиц events и check_results согласно
// конфигу инстанса. Вызывается при каждом старте: ретеншн — свойство
// инсталляции, не миграции.
func ApplyRetention(ctx context.Context, conn driver.Conn, days int) error {
	for _, table := range []string{"events", "check_results"} {
		var ddl string
		if err := conn.QueryRow(ctx, "SHOW CREATE TABLE "+table).Scan(&ddl); err != nil {
			return fmt.Errorf("apply retention: read ddl %s: %w", table, err)
		}
		// ALTER ... MODIFY TTL запускает мутацию таблицы — не дёргаем её
		// на каждом старте, если TTL уже совпадает.
		if !needsRetention(ddl, days) {
			continue
		}
		q := fmt.Sprintf(
			"ALTER TABLE %s MODIFY TTL toDateTime(timestamp) + INTERVAL %d DAY", table, days)
		if err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("apply retention %s: %w", table, err)
		}
	}
	return nil
}

// migrationLockKey — ключ PG advisory lock, сериализующего миграции
// (в т.ч. ClickHouse-миграции, у которых нет своего межпроцессного лока).
const migrationLockKey int64 = 0x676f7463686101

// WithMigrationLock выполняет fn под session-level advisory lock в PG.
// Реплики, стартующие одновременно, применяют миграции строго по очереди.
func WithMigrationLock(ctx context.Context, pool *pgxpool.Pool, fn func() error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migration lock: acquire conn: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()
	return fn()
}

// MigrateDownPG откатывает все PG-миграции. Используется тестами
// up-down-up; в проде не вызывается.
func MigrateDownPG(dsn string) error {
	src, err := iofs.New(pgMigrations, "migrations/pg")
	if err != nil {
		return fmt.Errorf("migrations source pg: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, pgx5URL(dsn))
	if err != nil {
		return fmt.Errorf("migrate init pg: %w", err)
	}
	defer m.Close()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down pg: %w", err)
	}
	return nil
}

// needsRetention: TTL в SHOW CREATE TABLE ClickHouse нормализован
// в toIntervalDay(N) — сравниваем с желаемым значением.
func needsRetention(ddl string, days int) bool {
	return !strings.Contains(ddl, fmt.Sprintf("toIntervalDay(%d)", days))
}
