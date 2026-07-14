package db_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestMigratePG(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("MigratePG: %v", err)
	}
	// Идемпотентность: повторный прогон не ошибка.
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("MigratePG (second run): %v", err)
	}

	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer pool.Close()
	var n int
	err = pool.QueryRow(ctx,
		"SELECT count(*) FROM pg_extension WHERE extname = 'citext'").Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("citext extension not installed: n=%d err=%v", n, err)
	}
}

func TestMigrateCHAndRetention(t *testing.T) {
	dsn := testenv.ClickHouseDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("MigrateCH: %v", err)
	}
	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("MigrateCH (second run): %v", err)
	}

	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("NewClickHouse: %v", err)
	}
	defer conn.Close()

	showCreate := func() string {
		var ddl string
		if err := conn.QueryRow(ctx, "SHOW CREATE TABLE events").Scan(&ddl); err != nil {
			t.Fatalf("SHOW CREATE TABLE events: %v", err)
		}
		return ddl
	}

	// ClickHouse desugars "INTERVAL N DAY" into "toIntervalDay(N)" at parse
	// time; SHOW CREATE TABLE reflects the parsed AST, not the original
	// migration source text. This is server-side normalization, not a
	// property of the driver or of our SQL (which uses the INTERVAL syntax
	// verbatim, per spec §5).
	ddl := showCreate()
	for _, want := range []string{"event_id", "project_id", "issue_id", "toYYYYMM(timestamp)", "toIntervalDay(90)"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("events DDL missing %q:\n%s", want, ddl)
		}
	}

	if err := db.ApplyRetention(ctx, conn, 180); err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if ddl := showCreate(); !strings.Contains(ddl, "toIntervalDay(180)") {
		t.Errorf("TTL not updated to 180 days:\n%s", ddl)
	}
}

func TestWithMigrationLockSerializes(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer pool.Close()

	var mu sync.Mutex
	var inside, maxInside int
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.WithMigrationLock(ctx, pool, func() error {
				mu.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()
				time.Sleep(100 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("WithMigrationLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("critical section overlapped: max concurrent = %d", maxInside)
	}
}

func TestMigratePGUpDownUp(t *testing.T) {
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := db.MigrateDownPG(dsn); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func TestTenancySchema(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, table := range []string{
		"users", "sessions", "organizations", "org_members", "org_invites",
		"teams", "team_members", "projects", "project_teams", "project_keys",
	} {
		var n int
		err := pool.QueryRow(ctx,
			"SELECT count(*) FROM information_schema.tables WHERE table_name = $1", table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s: n=%d err=%v", table, n, err)
		}
	}
	// citext-уникальность email регистронезависима.
	_, err := pool.Exec(ctx,
		"INSERT INTO users (email, password_hash) VALUES ('A@b.c','x'), ('a@B.C','y')")
	if err == nil {
		t.Error("want unique violation for case-insensitive duplicate email")
	}
}
