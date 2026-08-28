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
	svc.SetKeyring(mustKeyring(t, "a-strong-master-key-for-channel-secrets"))
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

// TestChannelTrustedRoundTrip — отметка «получатель мой» переживает запись,
// чтение и правку. Дефолт при создании — false: политика деталей строится на
// умолчании «не доверяем», и новый канал не должен получать детали оттого, что
// поле забыли передать. Отдельно проверяется путь правки с ПУСТЫМ секретом —
// он идёт другим UPDATE, и отметка обязана доехать по обоим.
func TestChannelTrustedRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "chantrust")

	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "12345", Secret: "tok",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	chs, err := svc.Channels(ctx, pid)
	if err != nil || len(chs) != 1 {
		t.Fatalf("Channels = %+v err=%v, want one channel", chs, err)
	}
	if chs[0].Trusted {
		t.Fatal("новый канал доверенный по умолчанию — политика деталей обязана начинать с недоверия")
	}

	// Правка без секрета: тот же UPDATE, что и при исправлении опечатки.
	if err := svc.UpdateChannel(ctx, alert.Channel{
		ID: id, ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "12345", Trusted: true,
	}); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	if chs, err = svc.Channels(ctx, pid); err != nil || len(chs) != 1 || !chs[0].Trusted {
		t.Fatalf("после установки отметки = %+v err=%v, want Trusted", chs, err)
	}

	// И обратно, уже вместе со сменой секрета — второй UPDATE.
	if err := svc.UpdateChannel(ctx, alert.Channel{
		ID: id, ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "12345", Secret: "new-tok",
	}); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	if chs, err = svc.Channels(ctx, pid); err != nil || len(chs) != 1 || chs[0].Trusted {
		t.Fatalf("после снятия отметки = %+v err=%v, want !Trusted", chs, err)
	}

	// Создание с явной отметкой — тот же путь, что и форма с галочкой.
	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true, Target: "me@corp.example", Trusted: true,
	}); err != nil {
		t.Fatalf("CreateChannel trusted: %v", err)
	}
	chs, err = svc.Channels(ctx, pid)
	if err != nil || len(chs) != 2 || !chs[1].Trusted {
		t.Fatalf("созданный с отметкой = %+v err=%v, want Trusted", chs, err)
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
	svc.SetKeyring(mustKeyring(t, "a-strong-master-key-for-channel-secrets"))
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

// TestChannelEmailTargetNormalized — адрес с отображаемым именем сохраняется
// только своей адресной частью.
//
// mail.ParseAddress принимает «Ops Team <ops@corp.example>», и раньше эта
// строка попадала в базу целиком. Дальше ломались обе стороны сразу:
// отправитель кладёт Target прямо в SMTP-команду RCPT TO — сервер отвечает
// отказом, а политика раскрытия деталей видела домен «corp.example>» и не
// признавала его своим, даже если corp.example перечислен как доверенный.
func TestChannelEmailTargetNormalized(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "chantarget-norm")

	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true,
		Target: "Ops Team <ops@corp.example>",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	chs, err := svc.Channels(ctx, pid)
	if err != nil || len(chs) != 1 {
		t.Fatalf("Channels = %+v err=%v", chs, err)
	}
	if chs[0].Target != "ops@corp.example" {
		t.Fatalf("Target = %q, want адрес без отображаемого имени", chs[0].Target)
	}

	// Политика теперь видит нормальный домен и признаёт его своим.
	p := alert.NewDetailPolicy("https://gotcha.example", []string{"corp.example"}, false)
	if !p.AllowsDetails(chs[0]) {
		t.Error("доверенный домен не распознан после сохранения канала")
	}

	// Та же нормализация при правке.
	if err := svc.UpdateChannel(ctx, alert.Channel{
		ID: id, ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true,
		Target: "  Дежурный <duty@corp.example>  ",
	}); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	chs, err = svc.Channels(ctx, pid)
	if err != nil || len(chs) != 1 || chs[0].Target != "duty@corp.example" {
		t.Fatalf("Target после правки = %+v err=%v, want duty@corp.example", chs, err)
	}
}

// TestChannelWithBrokenSecretStaysVisible — канал с нечитаемым секретом
// остаётся в списке, помеченным, но не доставляет.
//
// Раньше он молча выпадал из выдачи Channels, и это делало его невидимым
// дважды: уведомления по нему переставали ставиться в очередь (ни следа в
// журнале доставок, ни отметки в интерфейсе — «тишина в Telegram»
// неотличима от «инцидентов не было»), а починить или удалить его из
// интерфейса было нельзя: проверка принадлежности строится поверх того же
// списка и отвечала 404.
func TestChannelWithBrokenSecretStaysVisible(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newEvalProject(t, pool, "brokensecret")

	// Канал заведён под одним мастер-ключом...
	svc := alert.NewService(pool)
	svc.SetKeyring(mustKeyring(t, "original-master-key-for-channel-secrets"))
	id, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true,
		Target: "-100500", Secret: "bot-token",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// ...а читается под другим — ровно то, что происходит при смене
	// GOTCHA_SECRET_KEY.
	rotated := alert.NewService(pool)
	rotated.SetKeyring(mustKeyring(t, "a-completely-different-master-key-value"))

	chs, err := rotated.Channels(ctx, pid)
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	if len(chs) != 1 {
		t.Fatalf("каналов %d, want 1 — канал с нечитаемым секретом пропал из списка", len(chs))
	}
	c := chs[0]
	if !c.SecretBroken {
		t.Error("канал не помечен как сломанный — интерфейс покажет его исправным")
	}
	if c.Secret != "" {
		t.Errorf("секрет = %q, want пустой: расшифровать его нечем", c.Secret)
	}
	if c.Deliverable() {
		t.Error("канал считается пригодным к доставке — уйдёт вебхук без подписи")
	}

	// Починить его можно: правка секрета работает, потому что канал виден.
	if err := rotated.UpdateChannel(ctx, alert.Channel{
		ID: id, ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true,
		Target: "-100500", Secret: "new-bot-token",
	}); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	chs, err = rotated.Channels(ctx, pid)
	if err != nil || len(chs) != 1 {
		t.Fatalf("Channels после правки = %+v err=%v", chs, err)
	}
	if chs[0].SecretBroken {
		t.Error("после перевыпуска секрета канал всё ещё помечен сломанным")
	}
	if !chs[0].Deliverable() {
		t.Error("после перевыпуска секрета канал не годится к доставке")
	}
}
