package db_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Флаг добавляется выключенным — скрытие чек-листа решает человек, а не миграция.
func TestMigrate0059DefaultsHideGettingStartedFalse(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 58); err != nil {
		t.Fatalf("migrate to 58: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var userID int64
	mustScan(t, pool, &userID,
		"INSERT INTO users (email, password_hash) VALUES ('m59@example.com', 'x') RETURNING id")

	if err := db.MigratePGTo(dsn, 59); err != nil {
		t.Fatalf("migrate to 59: %v", err)
	}

	var hidden bool
	var email string
	if err := pool.QueryRow(ctx,
		"SELECT email, hide_getting_started FROM users WHERE id = $1", userID).Scan(&email, &hidden); err != nil {
		t.Fatalf("select user после миграции: %v", err)
	}
	if email != "m59@example.com" {
		t.Fatalf("email = %q: строка задета миграцией", email)
	}
	if hidden {
		t.Fatal("hide_getting_started = true после миграции, want false")
	}
}
