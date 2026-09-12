package uptime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

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

func mustKeyring(t *testing.T, raw string) secretbox.Keyring {
	t.Helper()
	ring, err := secretbox.NewKeyring(raw, "")
	if err != nil {
		t.Fatalf("NewKeyring(%q): %v", raw, err)
	}
	return ring
}

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

func rawBytesOf(t *testing.T, pool *pgxpool.Pool, id int64) json.RawMessage {
	t.Helper()
	var raw json.RawMessage
	if err := pool.QueryRow(context.Background(), "SELECT config FROM monitors WHERE id = $1", id).Scan(&raw); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	return raw
}

func setStoredConfig(t *testing.T, pool *pgxpool.Pool, id int64, cfg uptime.HTTPConfig) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "UPDATE monitors SET config = $2 WHERE id = $1", id, mustJSON(t, cfg)); err != nil {
		t.Fatalf("set stored config: %v", err)
	}
}

func mustKeyringWithPrev(t *testing.T, current, previous string) secretbox.Keyring {
	t.Helper()
	ring, err := secretbox.NewKeyring(current, previous)
	if err != nil {
		t.Fatalf("NewKeyring(%q, %q): %v", current, previous, err)
	}
	return ring
}

func TestCreateEncryptsHeaderValuesAtRest(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "uptime-master-key-A2a"))
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
	svc.SetKeyring(mustKeyring(t, "uptime-master-key-A2a"))
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
	legacy := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := httpMonitorWithHeaders(t, pid, map[string]string{"Authorization": "Bearer legacy-plain"})
	created, err := legacy.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create legacy: %v", err)
	}

	stored := rawConfigOf(t, pool, created.ID)
	if strings.HasPrefix(stored.Headers["Authorization"], secretbox.EncPrefix) {
		t.Fatalf("legacy value unexpectedly encrypted: %q", stored.Headers["Authorization"])
	}

	keyed := uptime.NewService(pool)
	keyed.SetKeyring(mustKeyring(t, "uptime-master-key-A2a"))
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

func TestUpdateDoesNotDoubleEncryptHeaders(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "uptime-master-key-A2a"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := httpMonitorWithHeaders(t, pid, map[string]string{"Authorization": "Bearer no-double-seal"})
	created, err := svc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var encrypted json.RawMessage
	if err := pool.QueryRow(ctx, "SELECT config FROM monitors WHERE id = $1", created.ID).Scan(&encrypted); err != nil {
		t.Fatalf("read encrypted config: %v", err)
	}

	upd := created
	upd.Config = encrypted
	if err := svc.Update(ctx, upd, []string{"local"}, nil); err != nil {
		t.Fatalf("update: %v", err)
	}

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

func TestRewrapSecretsPromotesReadableValuesAndIsIdempotent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	// байт-в-байт совпадает с secretbox_test.go:v1FixedEnvelope — пакеты не
	// могут делить неэкспортируемую константу.
	const (
		v1Master   = "vector-master-v1-legacy-old-code"
		v1Plain    = "legacy-v1-secret-value"
		v1Envelope = "enc:AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYudf0xP3/sKnysGe0CDB7Uzw42DGYRgM/gl3FF8KMFQgpVnZw4I4="
	)
	const oldMaster = "rewrap-test-old-master-key"

	oldRing := mustKeyring(t, oldMaster)
	v2PrevEnvelope, err := oldRing.Seal("v2-prev-secret")
	if err != nil {
		t.Fatalf("seal with old ring: %v", err)
	}

	ring := mustKeyringWithPrev(t, v1Master, oldMaster)

	unkeyed := uptime.NewService(pool)
	legacyM, err := unkeyed.Create(ctx, httpMonitorWithHeaders(t, pid, map[string]string{
		"X-Legacy": "legacy-plaintext-secret",
	}), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create legacy monitor: %v", err)
	}

	v1M, err := unkeyed.Create(ctx, httpMonitorWithHeaders(t, pid, map[string]string{
		"X-V1": "placeholder",
	}), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create v1 monitor: %v", err)
	}
	setStoredConfig(t, pool, v1M.ID, uptime.HTTPConfig{
		Method:  "GET",
		URL:     "https://example.com/health",
		Headers: map[string]string{"X-V1": v1Envelope},
	})

	v2PrevM, err := unkeyed.Create(ctx, httpMonitorWithHeaders(t, pid, map[string]string{
		"X-V2Prev": "placeholder",
	}), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create v2-prev monitor: %v", err)
	}
	setStoredConfig(t, pool, v2PrevM.ID, uptime.HTTPConfig{
		Method:  "GET",
		URL:     "https://example.com/health",
		Headers: map[string]string{"X-V2Prev": v2PrevEnvelope},
	})

	svc := uptime.NewService(pool)
	svc.SetKeyring(ring)

	n, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if n != 3 {
		t.Fatalf("rewrap updated %d monitors, want 3", n)
	}

	curTag := secretbox.EncPrefix + "v2:" + ring.CurrentID() + ":"
	cases := []struct {
		id     int64
		header string
		plain  string
	}{
		{legacyM.ID, "X-Legacy", "legacy-plaintext-secret"},
		{v1M.ID, "X-V1", v1Plain},
		{v2PrevM.ID, "X-V2Prev", "v2-prev-secret"},
	}
	for _, c := range cases {
		stored := rawConfigOf(t, pool, c.id).Headers[c.header]
		if !strings.HasPrefix(stored, curTag) {
			t.Fatalf("%s: stored value %q not promoted to current key envelope %q", c.header, stored, curTag)
		}
		got, err := svc.Get(ctx, c.id)
		if err != nil {
			t.Fatalf("%s: get: %v", c.header, err)
		}
		var cfg uptime.HTTPConfig
		if err := json.Unmarshal(got.Config, &cfg); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.header, err)
		}
		if cfg.Headers[c.header] != c.plain {
			t.Fatalf("%s: decrypts to %q, want %q", c.header, cfg.Headers[c.header], c.plain)
		}
	}

	n2, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("second rewrap: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second rewrap updated %d, want 0 (идемпотентность)", n2)
	}
}

func TestRewrapSecretsNoKey(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	svc := uptime.NewService(pool)
	created, err := svc.Create(ctx, httpMonitorWithHeaders(t, pid, map[string]string{
		"X-Plain": "plaintext-header-value",
	}), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	before := rawBytesOf(t, pool, created.ID)

	updated, err := svc.RewrapSecrets(ctx)
	if err != nil || updated != 0 {
		t.Fatalf("RewrapSecrets без ключа = (%d,%v), want (0,nil)", updated, err)
	}

	after := rawBytesOf(t, pool, created.ID)
	if string(before) != string(after) {
		t.Fatalf("config изменён без ключа: before=%s after=%s", before, after)
	}
}

func TestRewrapSecretsPoolClosed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "rewrap-poolclosed-master"))
	pool.Close()

	updated, err := svc.RewrapSecrets(context.Background())
	if err == nil {
		t.Fatalf("RewrapSecrets на закрытом пуле = (%d,nil), want ненулевую ошибку", updated)
	}
}

func TestRewrapSecretsDegradesPerHeaderValueNotPerRow(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	lostRing := mustKeyring(t, "lost-master-nobody-has-this-key")
	lostEnvelope, err := lostRing.Seal("unreachable-secret")
	if err != nil {
		t.Fatalf("seal with lost ring: %v", err)
	}

	unkeyed := uptime.NewService(pool)
	m, err := unkeyed.Create(ctx, httpMonitorWithHeaders(t, pid, map[string]string{
		"X-Readable": "placeholder", "X-Lost": "placeholder",
	}), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	setStoredConfig(t, pool, m.ID, uptime.HTTPConfig{
		Method: "GET",
		URL:    "https://example.com/health",
		Headers: map[string]string{
			"X-Readable": "readable-plaintext",
			"X-Lost":     lostEnvelope,
		},
	})

	svc := uptime.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "degrade-test-current-master"))

	n, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("rewrap must not fail on unreadable header: %v", err)
	}
	if n != 1 {
		t.Fatalf("rewrap updated %d monitors, want 1", n)
	}

	stored := rawConfigOf(t, pool, m.ID)
	if !strings.HasPrefix(stored.Headers["X-Readable"], secretbox.EncPrefix) {
		t.Fatalf("readable header not promoted: %q", stored.Headers["X-Readable"])
	}
	if stored.Headers["X-Lost"] != lostEnvelope {
		t.Fatalf("unreadable header must be left untouched, was %q now %q", lostEnvelope, stored.Headers["X-Lost"])
	}
}

func TestRewrapSecretsScopedToHTTPKind(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	unkeyed := uptime.NewService(pool)
	m := baseHTTPMonitor(pid)
	m.Kind = uptime.KindTCP
	m.Config = tcpConfig(t, uptime.TCPConfig{Host: "example.com", Port: 443})
	created, err := unkeyed.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create tcp monitor: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE monitors SET config = $2 WHERE id = $1", created.ID,
		json.RawMessage(`{"host":"example.com","port":443,"headers":{"X-Odd":"plaintext-in-non-http-config"}}`)); err != nil {
		t.Fatalf("set tcp config with headers-shaped field: %v", err)
	}
	before := rawBytesOf(t, pool, created.ID)

	svc := uptime.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "scope-test-master"))
	if _, err := svc.RewrapSecrets(ctx); err != nil {
		t.Fatalf("rewrap: %v", err)
	}

	after := rawBytesOf(t, pool, created.ID)
	if string(before) != string(after) {
		t.Fatalf("tcp monitor config touched by RewrapSecrets: before=%s after=%s", before, after)
	}
}

func TestRewrapSecretsSkipsHTTPConfigWithoutHeaders(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	unkeyed := uptime.NewService(pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created, err := unkeyed.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	before := rawBytesOf(t, pool, created.ID)

	svc := uptime.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "no-headers-test-master"))
	n, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if n != 0 {
		t.Fatalf("rewrap updated %d monitors, want 0 (нет заголовков)", n)
	}

	after := rawBytesOf(t, pool, created.ID)
	if string(before) != string(after) {
		t.Fatalf("config without headers was rewritten: before=%s after=%s", before, after)
	}
}

func TestRewrapSecretsDoesNotSealEmptyHeaderValue(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	unkeyed := uptime.NewService(pool)
	created, err := unkeyed.Create(ctx, httpMonitorWithHeaders(t, pid, map[string]string{"X-Empty": ""}), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	before := rawBytesOf(t, pool, created.ID)

	svc := uptime.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "empty-header-test-master"))
	n, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if n != 0 {
		t.Fatalf("rewrap updated %d monitors, want 0 (пустое значение не шифруется)", n)
	}

	after := rawBytesOf(t, pool, created.ID)
	if string(before) != string(after) {
		t.Fatalf("config with empty header value was rewritten: before=%s after=%s", before, after)
	}
}

func TestRewrapSecretsCapsUnreadableLogAtFive(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	const total = 7
	lostRing := mustKeyring(t, "log-cap-test-lost-master")
	unkeyed := uptime.NewService(pool)
	for i := 0; i < total; i++ {
		sealed, err := lostRing.Seal("unreachable-secret")
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		m, err := unkeyed.Create(ctx, httpMonitorWithHeaders(t, pid, map[string]string{"X-Lost": "placeholder"}), []string{"local"}, nil)
		if err != nil {
			t.Fatalf("create monitor %d: %v", i, err)
		}
		setStoredConfig(t, pool, m.ID, uptime.HTTPConfig{
			Method:  "GET",
			URL:     "https://example.com/health",
			Headers: map[string]string{"X-Lost": sealed},
		})
	}

	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prevLogger)

	svc := uptime.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "log-cap-test-current-master"))
	n, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if n != 0 {
		t.Fatalf("rewrap updated %d, want 0 (все значения нечитаемы)", n)
	}

	logged := strings.Count(buf.String(), "header value unreadable")
	if logged != 5 {
		t.Fatalf("logged %d unreadable-header lines, want cap of 5 (total unreadable=%d): %s", logged, total, buf.String())
	}
	if !strings.Contains(buf.String(), "unreadable_skipped=7") {
		t.Fatalf("summary line must report all %d unreadable, got: %s", total, buf.String())
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
