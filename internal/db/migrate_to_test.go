package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigratePGToStopsAtVersion — пошаговая миграция нужна, чтобы проверять
// миграции на НЕПУСТОЙ базе: засеять данные старой схемой и применить
// следующую. Без неё любой тест миграции работает на пустых таблицах, где
// проходит и то, что на живой базе падает (ADD COLUMN NOT NULL без DEFAULT).
func TestMigratePGToStopsAtVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 2); err != nil {
		t.Fatalf("migrate to 2: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	var version int64
	var dirty bool
	if err := pool.QueryRow(context.Background(),
		"SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != 2 || dirty {
		t.Fatalf("version = %d dirty = %v, want 2 false", version, dirty)
	}

	// Таблица из миграции 0002 есть, из 0003 — ещё нет.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name = 'issues'`).Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if n != 0 {
		t.Fatalf("таблица issues существует после миграции до 0002 — остановки не произошло")
	}
}
