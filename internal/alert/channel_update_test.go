package alert_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestUpdateChannelKeepsSecret — пустой секрет в форме правки означает
// «оставить прежний». Существует потому, что секрет вводится вслепую
// (type=password) и в форму не возвращается: если бы пустое поле затирало его,
// правка опечатки в адресе молча ломала бы доставку в Telegram.
func TestUpdateChannelKeepsSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	svc.SetSecretKey("a-strong-master-key-for-channel-secrets")
	ctx := context.Background()
	pid := newEvalProject(t, pool, "chanupd")

	const secret = "bot-token-KEEP-me"
	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: false, Target: "12345", Secret: secret,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	if err := svc.UpdateChannel(ctx, alert.Channel{
		ID: id, ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "67890",
	}); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}

	chs, err := svc.Channels(ctx, pid)
	if err != nil || len(chs) != 1 {
		t.Fatalf("Channels = %+v err=%v, want one channel", chs, err)
	}
	if chs[0].Target != "67890" || !chs[0].Enabled {
		t.Fatalf("channel after update = %+v, want target 67890 and enabled", chs[0])
	}
	if chs[0].Secret != secret {
		t.Fatalf("secret after update = %q, want kept %q", chs[0].Secret, secret)
	}
}

// TestUpdateChannelReplacesSecret — непустой секрет заменяет прежний и, как при
// создании, ложится в базу зашифрованным, а не открытым текстом.
func TestUpdateChannelReplacesSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	svc.SetSecretKey("a-strong-master-key-for-channel-secrets")
	ctx := context.Background()
	pid := newEvalProject(t, pool, "chanupd2")

	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "12345", Secret: "old-token",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	const rotated = "bot-token-ROTATED"
	if err := svc.UpdateChannel(ctx, alert.Channel{
		ID: id, ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "12345", Secret: rotated,
	}); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}

	var raw string
	if err := pool.QueryRow(ctx, "SELECT secret FROM alert_channels WHERE id=$1", id).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !strings.HasPrefix(raw, "enc:") || strings.Contains(raw, rotated) {
		t.Fatalf("rotated secret not encrypted at rest: %q", raw)
	}
	chs, err := svc.Channels(ctx, pid)
	if err != nil || len(chs) != 1 || chs[0].Secret != rotated {
		t.Fatalf("Channels = %+v err=%v, want secret %q", chs, err, rotated)
	}
}

// TestUpdateChannelScopedToProject — id канала приходит из формы, поэтому
// project_id стоит в условии UPDATE. Без него владелец одного проекта правил бы
// канал соседнего, подобрав id.
func TestUpdateChannelScopedToProject(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx := context.Background()
	mine := newEvalProject(t, pool, "chanscope-mine")
	theirs := newEvalProject(t, pool, "chanscope-theirs")

	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: theirs, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	err = svc.UpdateChannel(ctx, alert.Channel{
		ID: id, ProjectID: mine, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://evil.example/hook",
	})
	if !errors.Is(err, alert.ErrNotFound) {
		t.Fatalf("UpdateChannel across projects = %v, want ErrNotFound", err)
	}

	chs, err := svc.Channels(ctx, theirs)
	if err != nil || len(chs) != 1 || chs[0].Target != "https://example.com/hook" {
		t.Fatalf("victim channel = %+v err=%v, want untouched", chs, err)
	}
}

// TestUpdateChannelValidates — правка проходит ту же валидацию, что и создание:
// иначе адрес можно было бы испортить через правку в обход проверок.
func TestUpdateChannelValidates(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "chanvalid")

	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	err = svc.UpdateChannel(ctx, alert.Channel{
		ID: id, ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "not-a-url",
	})
	if !errors.Is(err, alert.ErrInvalidChannel) {
		t.Fatalf("UpdateChannel with bad target = %v, want ErrInvalidChannel", err)
	}
}

// TestChannelTelegramTargetMustBeChatID — chat_id проверяется как целое число.
// Найдено на приёмке: правка Telegram-канала приняла получателя «не-урл» и
// сохранила его, а узнать об этом можно было только из лога неудачных доставок
// — то есть уже после того, как алерт не пришёл.
func TestChannelTelegramTargetMustBeChatID(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "chattarget")

	for _, target := range []string{"не-урл", "my chat", "", " 12345", "12345 "} {
		_, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: target, Secret: "bot-token",
		})
		if !errors.Is(err, alert.ErrInvalidChannel) {
			t.Errorf("CreateChannel with chat_id %q = %v, want ErrInvalidChannel", target, err)
		}
	}

	// Групповой chat_id отрицательный — он должен проходить.
	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "-1001234567890", Secret: "bot-token",
	})
	if err != nil {
		t.Fatalf("CreateChannel with group chat_id: %v", err)
	}

	// Правка проходит ту же проверку.
	if err := svc.UpdateChannel(ctx, alert.Channel{
		ID: id, ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "не-урл",
	}); !errors.Is(err, alert.ErrInvalidChannel) {
		t.Errorf("UpdateChannel with bad chat_id = %v, want ErrInvalidChannel", err)
	}
}
