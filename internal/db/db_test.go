package db_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestNewPostgres(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer pool.Close()

	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1: got %d, err %v", one, err)
	}
}

// TestNewPostgresPoolSettings — MaxConns и statement_timeout заданы явно, а
// не оставлены на встроенный дефолт pgxpool (W3-D, запись 7): max(4,
// runtime.NumCPU()) — 4-8 соединений на большинстве прод-хостов — тонко для
// пула, который делят ~150 HTTP-маршрутов, приём и 21 фоновый цикл. Без
// statement_timeout один зависший запрос удерживал бы соединение пула
// бесконечно.
func TestNewPostgresPoolSettings(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer pool.Close()

	if got := pool.Config().MaxConns; got != 20 {
		t.Errorf("MaxConns = %d, want 20", got)
	}

	var timeout string
	if err := pool.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout); err != nil {
		t.Fatalf("SHOW statement_timeout: %v", err)
	}
	if timeout != "30s" {
		t.Errorf("statement_timeout = %q, want %q", timeout, "30s")
	}
}

func TestNewClickHouse(t *testing.T) {
	dsn := testenv.ClickHouseDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	defer conn.Close()

	var one uint8
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1: got %d, err %v", one, err)
	}
}

func TestNewPostgresBadDSN(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := db.NewPostgres(ctx, "postgres://nobody@127.0.0.1:1/none"); err == nil {
		t.Fatal("want connection error, got nil")
	}
}
