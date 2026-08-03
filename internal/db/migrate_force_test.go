package db_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestForcePG — контракт db.ForcePG: снимает dirty-флаг, но только в двух
// разрешённых точках — текущая версия (миграция доделана руками) и текущая−1
// (миграция откачена руками). Всё остальное — опечатка, которая молча сдвинула
// бы точку отсчёта всех будущих миграций.
func TestForcePG(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	current, dirty, err := db.SchemaVersion(dsn)
	if err != nil || dirty {
		t.Fatalf("schema version: v=%d dirty=%v err=%v", current, dirty, err)
	}

	// Чистая база: force обязан отказать, а не «поправить» здоровую схему.
	if err := db.ForcePG(dsn, current); err == nil || !strings.Contains(err.Error(), "not dirty") {
		t.Fatalf("force on clean schema: err=%v, want «not dirty»", err)
	}

	setDirtyPG := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, "UPDATE schema_migrations SET dirty = true"); err != nil {
			t.Fatalf("set dirty: %v", err)
		}
	}

	// Опечатка в номере: ошибка называет оба числа — запрошенное и текущее.
	setDirtyPG()
	err = db.ForcePG(dsn, current-5)
	if err == nil {
		t.Fatal("force to current-5 succeeded, want error")
	}
	for _, num := range []string{strconv.Itoa(int(current - 5)), strconv.Itoa(int(current))} {
		if !strings.Contains(err.Error(), num) {
			t.Errorf("error %q lacks number %s", err, num)
		}
	}

	// N = current: флаг снят, версия не изменилась.
	if err := db.ForcePG(dsn, current); err != nil {
		t.Fatalf("force to current: %v", err)
	}
	if v, d, err := db.SchemaVersion(dsn); err != nil || d || v != current {
		t.Fatalf("after force: v=%d dirty=%v err=%v, want v=%d dirty=false", v, d, err, current)
	}

	// N = current−1: ручной откат — версия сдвигается.
	setDirtyPG()
	if err := db.ForcePG(dsn, current-1); err != nil {
		t.Fatalf("force to current-1: %v", err)
	}
	if v, d, err := db.SchemaVersion(dsn); err != nil || d || v != current-1 {
		t.Fatalf("after force-1: v=%d dirty=%v err=%v, want v=%d dirty=false", v, d, err, current-1)
	}
}

// TestForceCH — тот же контракт для ClickHouse. Таблица schema_migrations
// CH-драйвера golang-migrate — журнал: каждая смена версии дописывает строку
// (version, dirty, sequence), читается последняя по sequence. Dirty
// выставляется здесь той же вставкой, какую оставила бы оборвавшаяся миграция.
func TestForceCH(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	ctx := context.Background()
	dsn := testenv.ClickHouseDSN(t)
	if err := db.MigrateCH(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conn, err := db.NewClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	chVersion := func() (uint, bool) {
		t.Helper()
		var version int64
		var dirty uint8
		err := conn.QueryRow(ctx,
			"SELECT version, dirty FROM schema_migrations ORDER BY sequence DESC LIMIT 1").
			Scan(&version, &dirty)
		if err != nil {
			t.Fatalf("read ch version: %v", err)
		}
		return uint(version), dirty != 0
	}
	current, dirty := chVersion()
	if dirty {
		t.Fatalf("fresh schema is dirty at %d", current)
	}

	if err := db.ForceCH(dsn, current); err == nil || !strings.Contains(err.Error(), "not dirty") {
		t.Fatalf("force on clean schema: err=%v, want «not dirty»", err)
	}

	setDirtyCH := func() {
		t.Helper()
		err := conn.Exec(ctx,
			"INSERT INTO schema_migrations (version, dirty, sequence) VALUES (?, 1, ?)",
			int64(current), uint64(time.Now().UnixNano()))
		if err != nil {
			t.Fatalf("set dirty: %v", err)
		}
	}

	setDirtyCH()
	err = db.ForceCH(dsn, current-5)
	if err == nil {
		t.Fatal("force to current-5 succeeded, want error")
	}
	for _, num := range []string{strconv.Itoa(int(current - 5)), strconv.Itoa(int(current))} {
		if !strings.Contains(err.Error(), num) {
			t.Errorf("error %q lacks number %s", err, num)
		}
	}

	if err := db.ForceCH(dsn, current); err != nil {
		t.Fatalf("force to current: %v", err)
	}
	if v, d := chVersion(); d || v != current {
		t.Fatalf("after force: v=%d dirty=%v, want v=%d dirty=false", v, d, current)
	}

	setDirtyCH()
	if err := db.ForceCH(dsn, current-1); err != nil {
		t.Fatalf("force to current-1: %v", err)
	}
	if v, d := chVersion(); d || v != current-1 {
		t.Fatalf("after force-1: v=%d dirty=%v, want v=%d dirty=false", v, d, current-1)
	}
}
