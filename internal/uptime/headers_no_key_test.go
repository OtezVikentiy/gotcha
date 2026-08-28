package uptime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestGetScrubsEncryptedHeadersWithoutMasterKey — воспроизводит W2/P1-7: монитор
// заведён под мастер-ключом (значения заголовков зашифрованы, enc:-ciphertext в
// БД), а читается сервисом БЕЗ ключа вовсе (откат GOTCHA_SECRET_KEY на
// dev-дефолт: main.go SetKeyring тогда не вызывается, secretKeySet остаётся
// false). Раньше decryptMonitorConfig был no-op при !secretKeySet и отдавал
// config как есть — сырой enc:base64... лежал бы в значении заголовка. Теперь
// такое значение обнуляется, а не отдаётся ciphertext'ом.
func TestGetScrubsEncryptedHeadersWithoutMasterKey(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	keyed := uptime.NewService(pool)
	keyed.SetKeyring(mustKeyring(t, "uptime-master-key-rollback"))
	m := httpMonitorWithHeaders(t, pid, map[string]string{"Authorization": "Bearer rollback-victim"})
	created, err := keyed.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Заголовок и правда зашифрован at rest.
	stored := rawConfigOf(t, pool, created.ID)
	if got := stored.Headers["Authorization"]; got == "Bearer rollback-victim" {
		t.Fatalf("precondition: header must be encrypted at rest, got plaintext %q", got)
	}

	// Ключ откатился на dev-дефолт: новый сервис БЕЗ SetKeyring.
	noKey := uptime.NewService(pool)
	got, err := noKey.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get without key: %v", err)
	}
	var cfg uptime.HTTPConfig
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v := cfg.Headers["Authorization"]; v != "" {
		t.Fatalf("Authorization header = %q, want empty: ciphertext must not leak raw without a key", v)
	}
}

// TestLeaseScrubsEncryptedHeadersWithoutMasterKey — тот же откат ключа, но по
// пути lease → checker (check_http.go шлёт значения заголовков в исходящий
// запрос): монитор должен уйти на проверку БЕЗ ciphertext в заголовке, а не с
// enc:base64... вместо bearer-токена.
func TestLeaseScrubsEncryptedHeadersWithoutMasterKey(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	keyed := uptime.NewService(pool)
	keyed.SetKeyring(mustKeyring(t, "uptime-master-key-rollback"))
	m := httpMonitorWithHeaders(t, pid, map[string]string{"Authorization": "Bearer rollback-victim"})
	created, err := keyed.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	noKey := uptime.NewService(pool)
	if _, err := noKey.Schedule(ctx); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	jobs, err := noKey.LeaseLocal(ctx, "local", 10)
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
		if v := cfg.Headers["Authorization"]; v != "" {
			t.Fatalf("leased checker got raw ciphertext header: %q", v)
		}
	}
	if !found {
		t.Fatalf("monitor %d not leased", created.ID)
	}
}
