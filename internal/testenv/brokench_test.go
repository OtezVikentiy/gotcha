package testenv

import (
	"context"
	"testing"
	"time"
)

// TestBrokenCHFailsFast: соединение от BrokenCH обязано отвечать ошибкой на
// запрос быстро — иначе тесты деградации web-страниц ждали бы dial-таймаут
// на каждом обращении к ClickHouse (страница делает их по несколько).
func TestBrokenCHFailsFast(t *testing.T) {
	conn := BrokenCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	rows, err := conn.Query(ctx, "SELECT 1")
	if err == nil {
		rows.Close()
		t.Fatalf("query on broken connection succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("broken connection failed in %v, want well under 3s", elapsed)
	}
	if err := conn.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatalf("exec on broken connection succeeded")
	}
}

// TestBrokenCHCountingOneAttemptPerQuery: у BrokenCHCounting каждый запрос —
// ровно одна попытка подключения (драйвер не ретраит рукопожатие), иначе по
// счётчику нельзя было бы судить, сколько раз страница ходила в ClickHouse.
func TestBrokenCHCountingOneAttemptPerQuery(t *testing.T) {
	conn, attempts := BrokenCHCounting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const n = 3
	for i := 0; i < n; i++ {
		rows, err := conn.Query(ctx, "SELECT 1")
		if err == nil {
			rows.Close()
			t.Fatalf("query %d on broken connection succeeded", i)
		}
	}
	if got := attempts.Load(); got != n {
		t.Fatalf("connection attempts = %d for %d queries, want exactly %d (one per query)", got, n, n)
	}
}
