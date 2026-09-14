package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Миграция аддитивна, но накатывается на базу с уже существующими пользователями и сессиями —
// проверяем, что накат их не задевает и что FK/каскад на новой таблице реально работают.
func TestMigrate0097PasswordResets(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 96); err != nil {
		t.Fatalf("migrate to 96: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var userID int64
	mustScan(t, pool, &userID,
		"INSERT INTO users (email, password_hash) VALUES ('m97@example.com', 'x') RETURNING id")
	mustExec(t, pool,
		"INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ('\\x01', $1, now() + interval '1 day')", userID)

	if err := db.MigratePGTo(dsn, 97); err != nil {
		t.Fatalf("migrate to 97: %v", err)
	}

	var sessionCount int64
	mustScan(t, pool, &sessionCount, "SELECT count(*) FROM sessions WHERE user_id = $1", userID)
	if sessionCount != 1 {
		t.Fatalf("существовавшая сессия не пережила миграцию 97, count=%d, want 1", sessionCount)
	}

	var resetID int64
	mustScan(t, pool, &resetID, `
		INSERT INTO password_resets (user_id, token_hash, expires_at)
		VALUES ($1, '\x02', now() + interval '1 hour') RETURNING id`, userID)

	mustExec(t, pool, "DELETE FROM users WHERE id = $1", userID)

	var resetCount int64
	mustScan(t, pool, &resetCount, "SELECT count(*) FROM password_resets WHERE id = $1", resetID)
	if resetCount != 0 {
		t.Fatalf("password_resets пережила удаление пользователя, count=%d, want 0 (ON DELETE CASCADE)", resetCount)
	}
}

func TestMigrate0097DownDropsTable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 97); err != nil {
		t.Fatalf("migrate to 97: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var userID int64
	mustScan(t, pool, &userID,
		"INSERT INTO users (email, password_hash) VALUES ('m97-down@example.com', 'x') RETURNING id")
	mustExec(t, pool, `
		INSERT INTO password_resets (user_id, token_hash, expires_at)
		VALUES ($1, '\x03', now() + interval '1 hour')`, userID)

	if err := db.MigratePGTo(dsn, 96); err != nil {
		t.Fatalf("migrate down to 96 with a live row present: %v", err)
	}

	var exists bool
	mustScanBool(t, pool, &exists,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'password_resets')")
	if exists {
		t.Fatal("password_resets всё ещё существует после отката до 96")
	}
}

func mustScanBool(t *testing.T, pool *pgxpool.Pool, dst *bool, sql string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
}
