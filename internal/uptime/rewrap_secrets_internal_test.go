package uptime

// В package uptime (не uptime_test), потому что тест зовёт
// casUpdateMonitorConfig напрямую — неэкспортируемый метод, до которого
// внешнему тестовому пакету не дотянуться (см. пояснение в
// watchdog_concurrency_test.go). Помощники того файла (newConcurrencyTestProject,
// concurrencyTestHTTPMonitor) переиспользуются — тот же пакет.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestCASUpdateMonitorConfigRejectsStaleOldValue — фундамент race-safety
// RewrapSecrets: casUpdateMonitorConfig обязан отказать (0 затронутых
// строк, БЕЗ ошибки), если config в таблице уже не совпадает со значением,
// которое вызывающий читал ранее — иначе бэкфилл затирал бы правку,
// приехавшую в параллели (см. RewrapSecrets, service.go).
//
// Реальная гонка по времени между SELECT и UPDATE внутри одного вызова
// RewrapSecrets недетерминированна и не годится для теста; вместо неё здесь
// напрямую воспроизводится момент «между чтением и записью значение
// сменилось» — прямым SQL UPDATE между двумя вызовами casUpdateMonitorConfig,
// который этот же метод и обязан поймать.
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

	// «Конкурентный редактор» меняет config между чтением (staleCfg) и нашей
	// попыткой записи — прямым SQL, в обход сервиса.
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

	// С правильным (актуальным) старым значением обновление проходит.
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

// TestCASUpdateMonitorConfigExecError — обрыв соединения на самом UPDATE:
// casUpdateMonitorConfig обязан вернуть ошибку вызывающему, а не (false,nil)
// — иначе RewrapSecrets молча спишет реальный сбой записи на «конфиг
// изменили между чтением и записью» (CAS miss, 0 затронутых строк) и не
// залогирует его. Зеркало internal/alert.TestRewrapChannelSecretExecError.
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

// rawConfigBytes читает config монитора напрямую из БД — канонический вид
// jsonb, как его реально сравнивает CAS-предикат casUpdateMonitorConfig.
func rawConfigBytes(t *testing.T, pool *pgxpool.Pool, id int64) json.RawMessage {
	t.Helper()
	var raw json.RawMessage
	if err := pool.QueryRow(context.Background(), "SELECT config FROM monitors WHERE id = $1", id).Scan(&raw); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	return raw
}
