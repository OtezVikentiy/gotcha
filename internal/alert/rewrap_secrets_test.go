package alert_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Совпадает с вектором internal/secretbox/secretbox_test.go; скопирован, не получен через
// Keyring — у v1 нет id ключа, и запечатывать v1 в продуктовом коде больше нельзя.
const (
	rewrapV1Master   = "vector-master-v1-legacy-old-code"
	rewrapV1Plain    = "legacy-v1-secret-value"
	rewrapV1Envelope = "enc:AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYudf0xP3/sKnysGe0CDB7Uzw42DGYRgM/gl3FF8KMFQgpVnZw4I4="
)

// В отличие от CreateChannel, ничего не запечатывает — тест управляет точным
// содержимым secret (plaintext/v1/v2-prev/v2-current/битый).
func insertChannel(t *testing.T, pool *pgxpool.Pool, pid int64, secret string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO alert_channels (project_id, kind, enabled, target, secret)
		VALUES ($1, 'webhook', true, 'https://example.com/hook', $2) RETURNING id`,
		pid, secret).Scan(&id)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return id
}

func TestChannelsRewrapSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ring, err := secretbox.NewKeyring("rewrap-channels-current-master", rewrapV1Master)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	svc := alert.NewService(pool)
	svc.SetKeyring(ring)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "rewrapch")

	oldRing, err := secretbox.NewKeyring(rewrapV1Master, "")
	if err != nil {
		t.Fatalf("NewKeyring(old): %v", err)
	}
	v2Prev, err := oldRing.Seal("old-v2-secret")
	if err != nil {
		t.Fatalf("Seal(old): %v", err)
	}

	garbageRing, err := secretbox.NewKeyring("totally-unrelated-master-key-xyz", "")
	if err != nil {
		t.Fatalf("NewKeyring(garbage): %v", err)
	}
	garbage, err := garbageRing.Seal("garbage-secret")
	if err != nil {
		t.Fatalf("Seal(garbage): %v", err)
	}

	v2Current, err := ring.Seal("already-current-secret")
	if err != nil {
		t.Fatalf("Seal(current): %v", err)
	}

	plainID := insertChannel(t, pool, pid, "legacy-plaintext-secret")
	v1ID := insertChannel(t, pool, pid, rewrapV1Envelope)
	v2PrevID := insertChannel(t, pool, pid, v2Prev)
	v2CurID := insertChannel(t, pool, pid, v2Current)
	unreadableID := insertChannel(t, pool, pid, garbage)
	emptyID := insertChannel(t, pool, pid, "")

	updated, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("RewrapSecrets: %v", err)
	}
	if updated != 3 {
		t.Fatalf("RewrapSecrets updated = %d, want 3 (plain, v1, v2-prev)", updated)
	}

	readSecret := func(id int64) string {
		t.Helper()
		var stored string
		if err := pool.QueryRow(ctx, "SELECT secret FROM alert_channels WHERE id=$1", id).Scan(&stored); err != nil {
			t.Fatalf("read secret %d: %v", id, err)
		}
		return stored
	}
	wantCurrentV2 := func(id int64, wantOpen string) {
		t.Helper()
		stored := readSecret(id)
		if !strings.HasPrefix(stored, "enc:v2:"+ring.CurrentID()+":") {
			t.Fatalf("channel %d secret = %q, want v2 envelope with current key id %s", id, stored, ring.CurrentID())
		}
		got, err := ring.Open(stored)
		if err != nil || got != wantOpen {
			t.Fatalf("Open(channel %d) = (%q,%v), want (%q,nil)", id, got, err, wantOpen)
		}
	}
	wantCurrentV2(plainID, "legacy-plaintext-secret")
	wantCurrentV2(v1ID, rewrapV1Plain)
	wantCurrentV2(v2PrevID, "old-v2-secret")

	if got := readSecret(unreadableID); got != garbage {
		t.Fatalf("нечитаемый секрет изменён: %q, want unchanged %q", got, garbage)
	}
	// Пустой секрет НЕ запечатан — иначе сломался бы «оставить прежний» в UpdateChannel.
	if got := readSecret(emptyID); got != "" {
		t.Fatalf("пустой секрет запечатан: %q, want \"\"", got)
	}
	if got := readSecret(v2CurID); got != v2Current {
		t.Fatalf("v2-текущий секрет изменён: %q, want unchanged %q", got, v2Current)
	}

	updated2, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("RewrapSecrets (2nd pass): %v", err)
	}
	if updated2 != 0 {
		t.Fatalf("RewrapSecrets (2nd pass) updated = %d, want 0", updated2)
	}
}

func TestChannelsRewrapSecretsNoKey(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "rewrapchnokey")
	id := insertChannel(t, pool, pid, "plain-secret")

	updated, err := svc.RewrapSecrets(ctx)
	if err != nil || updated != 0 {
		t.Fatalf("RewrapSecrets без ключа = (%d,%v), want (0,nil)", updated, err)
	}
	var stored string
	if err := pool.QueryRow(ctx, "SELECT secret FROM alert_channels WHERE id=$1", id).Scan(&stored); err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored != "plain-secret" {
		t.Fatalf("secret изменён без ключа: %q", stored)
	}
}

// Не запускается с t.Parallel() — slog.SetDefault меняет глобальный логгер
// процесса.
type capturingLogHandler struct {
	records *[]slog.Record
}

func (h capturingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h capturingLogHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r.Clone())
	return nil
}
func (h capturingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h capturingLogHandler) WithGroup(string) slog.Handler      { return h }

func TestChannelsRewrapSecretsLogCap(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ring, err := secretbox.NewKeyring("rewrap-logcap-current-master", "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	svc := alert.NewService(pool)
	svc.SetKeyring(ring)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "rewraplogcap")

	garbageRing, err := secretbox.NewKeyring("logcap-unrelated-master-key-abc", "")
	if err != nil {
		t.Fatalf("NewKeyring(garbage): %v", err)
	}
	const unreadableCount = 8 // > rewrapLogCap(5)
	for i := 0; i < unreadableCount; i++ {
		garbage, err := garbageRing.Seal(fmt.Sprintf("garbage-secret-%d", i))
		if err != nil {
			t.Fatalf("Seal(garbage %d): %v", i, err)
		}
		insertChannel(t, pool, pid, garbage)
	}

	var records []slog.Record
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(capturingLogHandler{records: &records}))
	defer slog.SetDefault(prevDefault)

	updated, err := svc.RewrapSecrets(ctx)
	if err != nil {
		t.Fatalf("RewrapSecrets: %v", err)
	}
	if updated != 0 {
		t.Fatalf("RewrapSecrets updated = %d, want 0 (все секреты нечитаемы)", updated)
	}

	const wantSkipLogs = 5 // rewrapLogCap
	var skipLogs int
	var summarySeen bool
	for _, r := range records {
		switch r.Message {
		case "alert: channel secret cannot be rewrapped, skipping":
			skipLogs++
		case "alert: rewrap secrets backfill complete":
			summarySeen = true
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "unreadable" && a.Value.Int64() != int64(unreadableCount) {
					t.Fatalf("итоговый unreadable=%d, want %d (кап не должен резать итог)", a.Value.Int64(), unreadableCount)
				}
				return true
			})
		}
	}
	if skipLogs != wantSkipLogs {
		t.Fatalf("подробных логов нечитаемого секрета = %d, want %d (кап должен обрезать детализацию)", skipLogs, wantSkipLogs)
	}
	if !summarySeen {
		t.Fatalf("итоговая строка лога (alert: rewrap secrets backfill complete) не найдена")
	}
}

func TestChannelsRewrapSecretsPoolClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ring, err := secretbox.NewKeyring("rewrap-poolclosed-current-master", "")
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	svc := alert.NewService(pool)
	svc.SetKeyring(ring)
	pool.Close()

	updated, err := svc.RewrapSecrets(context.Background())
	if err == nil {
		t.Fatalf("RewrapSecrets на закрытом пуле = (%d,nil), want ненулевую ошибку", updated)
	}
}
