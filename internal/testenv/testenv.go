// Package testenv поднимает одноразовые PostgreSQL и ClickHouse в docker
// для интеграционных тестов. Все функции скипают тест при -short.
package testenv

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
)

const (
	postgresImage   = "postgres:17-alpine"
	clickhouseImage = "clickhouse/clickhouse-server:25.3-alpine"
)

// PostgresDSN запускает контейнер PostgreSQL и возвращает DSN.
// Контейнер гасится в t.Cleanup.
func PostgresDSN(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: skipped with -short")
	}
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("gotcha"),
		tcpostgres.WithUsername("gotcha"),
		tcpostgres.WithPassword("gotcha"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres dsn: %v", err)
	}
	return dsn
}

// ClickHouseDSN запускает контейнер ClickHouse и возвращает DSN
// вида clickhouse://user:pass@host:port/db.
func ClickHouseDSN(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: skipped with -short")
	}
	ctx := context.Background()
	ctr, err := tcclickhouse.Run(ctx, clickhouseImage,
		tcclickhouse.WithDatabase("gotcha"),
		tcclickhouse.WithUsername("gotcha"),
		tcclickhouse.WithPassword("gotcha"),
	)
	if err != nil {
		t.Fatalf("start clickhouse container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("clickhouse dsn: %v", err)
	}
	return dsn
}

// MigratedPG поднимает PostgreSQL, применяет все миграции и возвращает пул.
func MigratedPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect pg: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// MigratedCH поднимает ClickHouse, применяет миграции и возвращает соединение.
func MigratedCH(t *testing.T) driver.Conn {
	t.Helper()
	dsn := ClickHouseDSN(t)
	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("migrate ch: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("connect ch: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
