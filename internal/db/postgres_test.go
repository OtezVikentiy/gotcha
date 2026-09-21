package db_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Рестарт PostgreSQL рвёт уже открытые idle-соединения пула молча (сервер
// шлёт FATAL и закрывает сокет), а дефолтный ShouldPing pgxpool валидирует
// только соединения, простоявшие в пуле >=1с — то, что легло в пул мгновение
// назад, дефолт не ловит. Этим объясняется /register 500 при зелёном /readyz
// (задача 11, EL bare-metal e2e): readyz дошёл до живого соединения, реальный
// запрос — до мёртвого.
func TestPoolDropsDeadConnectionsOnAcquireEvenWhenRecentlyIdle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			var x int
			errs <- pool.QueryRow(ctx, "select 1").Scan(&x)
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("warmup: %v", err)
		}
	}
	time.Sleep(200 * time.Millisecond) // простой меньше 1с — порог дефолтного ShouldPing не достигнут

	admin, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		`select pg_terminate_backend(pid) from pg_stat_activity where datname = current_database() and pid <> pg_backend_pid()`,
	); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	admin.Release()
	time.Sleep(200 * time.Millisecond) // суммарный простой всё ещё меньше 1с

	errs2 := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			var x int
			errs2 <- pool.QueryRow(ctx, "select 1").Scan(&x)
		}()
	}
	var failed int
	for i := 0; i < n; i++ {
		if err := <-errs2; err != nil {
			failed++
			t.Logf("query on a connection killed by pg_terminate_backend: %v", err)
		}
	}
	if failed > 0 {
		t.Fatalf("%d/%d queries hit a dead pooled connection instead of being transparently replaced", failed, n)
	}
}
