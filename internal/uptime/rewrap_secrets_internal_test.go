package uptime

// в package uptime, а не uptime_test: тест зовёт неэкспортируемый
// casUpdateMonitorConfig напрямую; хелперы watchdog_concurrency_test.go общие.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// обязан отказать без ошибки, если config уже разошёлся со старым значением —
// иначе бэкфилл затёр бы правку; гонка воспроизведена прямым SQL UPDATE.
func TestCASUpdateMonitorConfigRejectsStaleOldValue(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newConcurrencyTestProject(t, pool)
	svc := NewService(pool)

	created, err := svc.Create(ctx, concurrencyTestHTTPMonitor(pid), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	staleCfg := rawConfigBytes(t, pool, created.ID)

	if _, err := pool.Exec(ctx, "UPDATE monitors SET config = $2 WHERE id = $1", created.ID,
		json.RawMessage(`{"method":"GET","url":"https://example.com/health","headers":{"X-Edited":"by-someone-else"}}`)); err != nil {
		t.Fatalf("simulate concurrent edit: %v", err)
	}
	concurrentCfg := rawConfigBytes(t, pool, created.ID) // канонический вид, как реально лежит в jsonb

	attemptedCfg := json.RawMessage(`{"method":"GET","url":"https://example.com/health","headers":{"X-Rewrapped":"target-value"}}`)

	ok, err := svc.casUpdateMonitorConfig(ctx, created.ID, attemptedCfg, staleCfg)
	if err != nil {
		t.Fatalf("cas update: %v", err)
	}
	if ok {
		t.Fatalf("cas update applied despite stale old value — race window not guarded")
	}
	if got := rawConfigBytes(t, pool, created.ID); !bytes.Equal(got, concurrentCfg) {
		t.Fatalf("row overwritten despite stale CAS: got %s, want unchanged %s", got, concurrentCfg)
	}

	ok2, err := svc.casUpdateMonitorConfig(ctx, created.ID, attemptedCfg, concurrentCfg)
	if err != nil {
		t.Fatalf("cas update (correct old value): %v", err)
	}
	if !ok2 {
		t.Fatalf("cas update with correct old value must apply")
	}
	var want json.RawMessage
	if err := pool.QueryRow(ctx, "SELECT $1::jsonb", attemptedCfg).Scan(&want); err != nil {
		t.Fatalf("canonicalize attemptedCfg: %v", err)
	}
	if got := rawConfigBytes(t, pool, created.ID); !bytes.Equal(got, want) {
		t.Fatalf("row after successful CAS = %s, want %s", got, want)
	}
}

// обрыв соединения обязан вернуть ошибку, не (false,nil) — иначе RewrapSecrets
// спишет реальный сбой записи на CAS-miss (конфиг просто «изменили»).
func TestCASUpdateMonitorConfigExecError(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	ctx := context.Background()
	pool.Close()

	ok, err := svc.casUpdateMonitorConfig(ctx, 1,
		json.RawMessage(`{"method":"GET","url":"https://example.com/health"}`),
		json.RawMessage(`{"method":"GET","url":"https://example.com/health"}`))
	if err == nil {
		t.Fatalf("casUpdateMonitorConfig на закрытом пуле = (%v,nil), want ненулевую ошибку", ok)
	}
	if ok {
		t.Fatalf("casUpdateMonitorConfig на закрытом пуле = (true,%v), want false при ошибке", err)
	}
}

// возвращает канонический jsonb, как его сравнивает CAS-предикат casUpdateMonitorConfig.
func rawConfigBytes(t *testing.T, pool *pgxpool.Pool, id int64) json.RawMessage {
	t.Helper()
	var raw json.RawMessage
	if err := pool.QueryRow(context.Background(), "SELECT config FROM monitors WHERE id = $1", id).Scan(&raw); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	return raw
}
