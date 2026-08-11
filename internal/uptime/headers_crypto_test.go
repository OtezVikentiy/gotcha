package uptime_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// httpMonitorWithHeaders — http-монитор с заданными заголовками.
func httpMonitorWithHeaders(t *testing.T, projectID int64, headers map[string]string) uptime.Monitor {
	t.Helper()
	m := baseHTTPMonitor(projectID)
	m.Config = httpConfig(t, uptime.HTTPConfig{
		Method:  "GET",
		URL:     "https://example.com/health",
		Headers: headers,
	})
	return m
}

// rawConfigOf читает config монитора напрямую из БД, минуя расшифровку сервиса,
// — чтобы проверить, что в хранилище значения заголовков лежат зашифрованными.
func rawConfigOf(t *testing.T, pool *pgxpool.Pool, id int64) uptime.HTTPConfig {
	t.Helper()
	var raw json.RawMessage
	if err := pool.QueryRow(context.Background(), "SELECT config FROM monitors WHERE id = $1", id).Scan(&raw); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	var cfg uptime.HTTPConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal raw config: %v", err)
	}
	return cfg
}

func TestCreateEncryptsHeaderValuesAtRest(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	svc.SetSecretKey("uptime-master-key-A2a")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := httpMonitorWithHeaders(t, pid, map[string]string{
		"Authorization": "Bearer s3cr3t-token",
		"X-Api-Key":     "topsecret-value",
	})
	created, err := svc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// В БД: имена заголовков видны, значения — с префиксом enc: и без plaintext.
	stored := rawConfigOf(t, pool, created.ID)
	if _, ok := stored.Headers["Authorization"]; !ok {
		t.Fatalf("header name Authorization missing in stored config: %+v", stored.Headers)
	}
	for name, val := range stored.Headers {
		if !strings.HasPrefix(val, secretbox.EncPrefix) {
			t.Fatalf("stored header %q value has no enc: prefix: %q", name, val)
		}
	}
	if strings.Contains(string(mustJSON(t, stored)), "s3cr3t-token") {
		t.Fatalf("plaintext secret leaked into stored config: %+v", stored.Headers)
	}

	// Get отдаёт расшифрованные значения (нужно форме A2b и живой проверке).
	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var gotCfg uptime.HTTPConfig
	if err := json.Unmarshal(got.Config, &gotCfg); err != nil {
		t.Fatalf("unmarshal got config: %v", err)
	}
	if gotCfg.Headers["Authorization"] != "Bearer s3cr3t-token" {
		t.Fatalf("Get did not decrypt Authorization: %q", gotCfg.Headers["Authorization"])
	}
	if gotCfg.Headers["X-Api-Key"] != "topsecret-value" {
		t.Fatalf("Get did not decrypt X-Api-Key: %q", gotCfg.Headers["X-Api-Key"])
	}
}

func TestLeaseDecryptsHeaderValuesForChecker(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	svc.SetSecretKey("uptime-master-key-A2a")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := httpMonitorWithHeaders(t, pid, map[string]string{"Authorization": "Bearer live-check-token"})
	created, err := svc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	jobs, err := svc.LeaseLocal(ctx, "local", 10)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	var found bool
	for _, j := range jobs {
		if j.MonitorID != created.ID {
			continue
		}
		found = true
		var cfg uptime.HTTPConfig
		if err := json.Unmarshal(j.Monitor.Config, &cfg); err != nil {
			t.Fatalf("unmarshal leased config: %v", err)
		}
		if cfg.Headers["Authorization"] != "Bearer live-check-token" {
			t.Fatalf("leased checker got non-decrypted header: %q", cfg.Headers["Authorization"])
		}
	}
	if !found {
		t.Fatalf("monitor %d not leased", created.ID)
	}
}

func TestLegacyPlaintextHeadersStillReadable(t *testing.T) {
	pool := testenv.MigratedPG(t)
	// Legacy-запись: сервис БЕЗ ключа сохраняет заголовки plaintext.
	legacy := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := httpMonitorWithHeaders(t, pid, map[string]string{"Authorization": "Bearer legacy-plain"})
	created, err := legacy.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create legacy: %v", err)
	}

	// В БД лежит plaintext (без enc:), совместимость.
	stored := rawConfigOf(t, pool, created.ID)
	if strings.HasPrefix(stored.Headers["Authorization"], secretbox.EncPrefix) {
		t.Fatalf("legacy value unexpectedly encrypted: %q", stored.Headers["Authorization"])
	}

	// Новый сервис С ключом читает legacy plaintext как есть (passthrough).
	keyed := uptime.NewService(pool)
	keyed.SetSecretKey("uptime-master-key-A2a")
	got, err := keyed.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get legacy: %v", err)
	}
	var cfg uptime.HTTPConfig
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatalf("unmarshal legacy config: %v", err)
	}
	if cfg.Headers["Authorization"] != "Bearer legacy-plain" {
		t.Fatalf("legacy plaintext not readable: %q", cfg.Headers["Authorization"])
	}
}

// TestUpdateDoesNotDoubleEncryptHeaders — идемпотентность: если в Update придёт
// config, чьи значения заголовков УЖЕ зашифрованы (enc:), повторного Seal быть
// не должно — иначе значение стало бы невосстановимым. Моделируем прямой
// UPDATE-путь: читаем зашифрованный config из БД (как отдал бы List/GetBatch) и
// подаём его же в Update.
func TestUpdateDoesNotDoubleEncryptHeaders(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	svc.SetSecretKey("uptime-master-key-A2a")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := httpMonitorWithHeaders(t, pid, map[string]string{"Authorization": "Bearer no-double-seal"})
	created, err := svc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Сырой (ещё зашифрованный) config из БД — именно такой отдают List/GetBatch.
	var encrypted json.RawMessage
	if err := pool.QueryRow(ctx, "SELECT config FROM monitors WHERE id = $1", created.ID).Scan(&encrypted); err != nil {
		t.Fatalf("read encrypted config: %v", err)
	}

	upd := created
	upd.Config = encrypted // подаём УЖЕ зашифрованный config обратно в Update
	if err := svc.Update(ctx, upd, []string{"local"}, nil); err != nil {
		t.Fatalf("update: %v", err)
	}

	// После Update значение по-прежнему расшифровывается в исходный plaintext —
	// повторного Seal не случилось.
	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	var cfg uptime.HTTPConfig
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Headers["Authorization"] != "Bearer no-double-seal" {
		t.Fatalf("double-seal corrupted value: %q", cfg.Headers["Authorization"])
	}
}

// TestEncryptLegacyHeadersBackfill — разовый бэкфилл дошифровывает заголовки
// монитора, сохранённого ДО включения шифрования (plaintext at-rest), не трогая
// уже зашифрованные и не переписывая записи без plaintext-заголовков.
// Идемпотентен: повторный вызов ничего не обновляет.
func TestEncryptLegacyHeadersBackfill(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	// Легаси: сервис БЕЗ ключа сохраняет заголовки plaintext.
	legacySvc := uptime.NewService(pool)
	legacyM, err := legacySvc.Create(ctx, httpMonitorWithHeaders(t, pid, map[string]string{
		"Authorization": "Bearer legacy-plaintext",
	}), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create legacy monitor: %v", err)
	}
	if got := rawConfigOf(t, pool, legacyM.ID).Headers["Authorization"]; got != "Bearer legacy-plaintext" {
		t.Fatalf("precondition: legacy header must be plaintext at rest, got %q", got)
	}

	// Теперь шифрование включено. Новый монитор рождается уже enc:.
	svc := uptime.NewService(pool)
	svc.SetSecretKey("backfill-master-key")
	encM, err := svc.Create(ctx, httpMonitorWithHeaders(t, pid, map[string]string{
		"Authorization": "Bearer born-encrypted",
	}), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create encrypted monitor: %v", err)
	}
	encBefore := rawConfigOf(t, pool, encM.ID).Headers["Authorization"]
	if !strings.HasPrefix(encBefore, secretbox.EncPrefix) {
		t.Fatalf("precondition: new monitor header must be enc:, got %q", encBefore)
	}

	// Бэкфилл: легаси-монитор дошифрован, уже-enc: — нетронут.
	n, err := svc.EncryptLegacyHeaders(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Fatalf("backfill updated %d monitors, want 1 (только легаси)", n)
	}
	if got := rawConfigOf(t, pool, legacyM.ID).Headers["Authorization"]; !strings.HasPrefix(got, secretbox.EncPrefix) {
		t.Fatalf("legacy header not encrypted after backfill: %q", got)
	}
	if got := rawConfigOf(t, pool, encM.ID).Headers["Authorization"]; got != encBefore {
		t.Fatalf("already-encrypted header must be untouched, was %q now %q", encBefore, got)
	}

	// Значение расшифровывается обратно в исходное (не двойное шифрование).
	got, err := svc.Get(ctx, legacyM.ID)
	if err != nil {
		t.Fatalf("get legacy: %v", err)
	}
	var cfg uptime.HTTPConfig
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Headers["Authorization"] != "Bearer legacy-plaintext" {
		t.Fatalf("backfilled header decrypts to %q, want original plaintext", cfg.Headers["Authorization"])
	}

	// Идемпотентность: второй прогон ничего не переписывает.
	n2, err := svc.EncryptLegacyHeaders(ctx)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second backfill updated %d, want 0 (идемпотентность)", n2)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
