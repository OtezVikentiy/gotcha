package alert_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// NewKeyring отказывает только на пустом current — здесь мастер-ключ всегда
// задан литералом.
func mustKeyring(t *testing.T, raw string) secretbox.Keyring {
	t.Helper()
	ring, err := secretbox.NewKeyring(raw, "")
	if err != nil {
		t.Fatalf("NewKeyring(%q): %v", raw, err)
	}
	return ring
}

func TestChannelSecretEncryptedAtRest(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "a-strong-master-key-for-channel-secrets"))
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

func TestChannelSecretByID(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "test-master-key-at-least-32-bytes-long"))
	ctx := context.Background()

	const token = "123456:AAHplaintext-bot-token"
	pid := newEvalProject(t, pool, "chansecbyid")
	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true,
		Target: "-100500", Secret: token,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	var stored string
	if err := pool.QueryRow(ctx, `SELECT secret FROM alert_channels WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if !strings.HasPrefix(stored, "enc:") {
		t.Fatalf("secret at rest = %q, want enc:-prefixed ciphertext", stored)
	}

	got, err := svc.ChannelSecret(ctx, id)
	if err != nil {
		t.Fatalf("ChannelSecret: %v", err)
	}
	if got != token {
		t.Fatalf("ChannelSecret = %q, want расшифрованный %q", got, token)
	}

	// Канал удалили между постановкой в очередь и отправкой — ErrNotFound, не
	// пустой секрет, с которым воркер молча ушёл бы слать в никуда.
	if err := svc.DeleteChannel(ctx, pid, id); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if _, err := svc.ChannelSecret(ctx, id); !errors.Is(err, alert.ErrNotFound) {
		t.Fatalf("ChannelSecret после удаления = %v, want ErrNotFound", err)
	}
}
