package uptime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// сервис без ключа (secretKeySet=false) обязан обнулять зашифрованное значение,
// а не отдавать сырой enc:base64... ciphertext вместо него.
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

	stored := rawConfigOf(t, pool, created.ID)
	if got := stored.Headers["Authorization"]; got == "Bearer rollback-victim" {
		t.Fatalf("precondition: header must be encrypted at rest, got plaintext %q", got)
	}

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

// тот же откат ключа, но по пути lease → checker: чекер не должен получить
// ciphertext вместо bearer-токена в исходящем запросе.
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
