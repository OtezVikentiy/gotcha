package alert_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestChannelSecretEncryptedAtRest — при заданном мастер-ключе секрет канала
// (Telegram bot-токен) хранится в БД зашифрованным (префикс enc:, plaintext не
// виден), а Channels() расшифровывает его обратно для доставки.
func TestChannelSecretEncryptedAtRest(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	svc.SetSecretKey("a-strong-master-key-for-channel-secrets")
	ctx := context.Background()
	pid := newEvalProject(t, pool, "chansec")

	const secret = "bot-token-SECRET-xyz"
	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "12345", Secret: secret,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	var raw string
	if err := pool.QueryRow(ctx, "SELECT secret FROM alert_channels WHERE id=$1", id).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !strings.HasPrefix(raw, "enc:") || strings.Contains(raw, secret) {
		t.Fatalf("channel secret not encrypted at rest: %q", raw)
	}

	chs, err := svc.Channels(ctx, pid)
	if err != nil || len(chs) != 1 || chs[0].Secret != secret {
		t.Fatalf("Channels decrypted = %+v err=%v, want secret %q", chs, err, secret)
	}
}
