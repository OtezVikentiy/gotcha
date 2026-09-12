package alert_test

import (
	"context"
	"errors"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestChannelEncryptedSecretWithoutMasterKey(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "nokeysecret")

	keyed := alert.NewService(pool)
	keyed.SetKeyring(mustKeyring(t, "a-strong-master-key-for-channel-secrets"))
	id, err := keyed.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true,
		Target: "-100500", Secret: "real-bot-token",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// SetKeyring не вызывается — имитирует откат GOTCHA_SECRET_KEY на dev.
	noKey := alert.NewService(pool)

	chs, err := noKey.Channels(ctx, pid)
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	if len(chs) != 1 {
		t.Fatalf("каналов %d, want 1", len(chs))
	}
	c := chs[0]
	if !c.SecretBroken {
		t.Error("канал не помечен SecretBroken — интерфейс покажет его исправным")
	}
	if c.Secret != "" {
		t.Errorf("Secret = %q, want пустой: ключа нет, ciphertext нечитаем", c.Secret)
	}
	if c.Deliverable() {
		t.Error("канал считается пригодным к доставке — уйдёт enc:-ciphertext вместо bot-токена")
	}

	if _, err := noKey.ChannelSecret(ctx, id); err == nil {
		t.Fatal("ChannelSecret вернул nil-ошибку — должен отказать, а не отдать сырой ciphertext")
	} else if errors.Is(err, alert.ErrNotFound) {
		t.Fatalf("ChannelSecret = %v, want НЕ ErrNotFound (канал существует, просто нечем расшифровать)", err)
	}
}

func TestChannelPlaintextSecretWithoutMasterKey(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "nokeyplain")

	noKey := alert.NewService(pool)
	id, err := noKey.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true,
		Target: "-100500", Secret: "plain-bot-token",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	chs, err := noKey.Channels(ctx, pid)
	if err != nil || len(chs) != 1 || chs[0].Secret != "plain-bot-token" || chs[0].SecretBroken {
		t.Fatalf("Channels = %+v err=%v, want неповреждённый plaintext-секрет", chs, err)
	}

	got, err := noKey.ChannelSecret(ctx, id)
	if err != nil || got != "plain-bot-token" {
		t.Fatalf("ChannelSecret = (%q,%v), want (plain-bot-token,nil)", got, err)
	}
}
