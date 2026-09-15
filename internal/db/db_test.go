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

// MaxConns и statement_timeout заданы явно, не на дефолте pgxpool: без таймаута один зависший запрос
// держал бы соединение пула бесконечно.
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

func TestNewClickHouseWriter(t *testing.T) {
	dsn := testenv.ClickHouseDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := db.NewClickHouseWriter(ctx, dsn)
	if err != nil {
		t.Fatalf("NewClickHouseWriter: %v", err)
	}
	defer conn.Close()

	var one uint8
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1: got %d, err %v", one, err)
	}
}

// writeDeadlineDialer подменяет DialContext писательского соединения — рукопожатие, сжатие
// и сама вставка батча обязаны пройти так же, как на обычном соединении.
func TestNewClickHouseWriterPrepareBatchRoundTrips(t *testing.T) {
	dsn := testenv.ClickHouseDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := db.NewClickHouseWriter(ctx, dsn)
	if err != nil {
		t.Fatalf("NewClickHouseWriter: %v", err)
	}
	defer conn.Close()

	const table = "batch_ctx_writer_roundtrip"
	if err := conn.Exec(ctx, "CREATE TABLE "+table+" (id UInt64) ENGINE = Memory"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	defer conn.Exec(context.Background(), "DROP TABLE "+table)

	batch, err := conn.PrepareBatch(db.BatchContext(ctx, db.WriteBudget), "INSERT INTO "+table+" (id)")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	for i := uint64(0); i < 3; i++ {
		if err := batch.Append(i); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var count uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+table).Scan(&count); err != nil || count != 3 {
		t.Fatalf("count() = %d, err %v, want 3", count, err)
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
