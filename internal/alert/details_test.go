package alert_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestDetailPolicyByRecipient — гейт трансграничной передачи решает по
// ПОЛУЧАТЕЛЮ, а не по транспорту.
//
// Прежнее правило (telegram/webhook — внешние, email — свой) ошибалось в обе
// стороны: ящик на публичном сервисе получал полный текст ошибки, потому что
// «это же email», а вебхук на собственный сервер деталей не получал, хотя
// вообще не покидал контура.
func TestDetailPolicyByRecipient(t *testing.T) {
	p := alert.NewDetailPolicy("https://gotcha.corp.example", nil, false)

	cases := []struct {
		name string
		ch   alert.Channel
		want bool
	}{
		{"почта на хосте инстанса", alert.Channel{Kind: alert.ChannelEmail, Target: "oncall@gotcha.corp.example"}, true},
		{"почта на поддомене хоста", alert.Channel{Kind: alert.ChannelEmail, Target: "a@mail.gotcha.corp.example"}, true},
		// Родительский домен САМ по себе не доверенный: подъём на уровень вверх
		// от хоста инстанса на public suffix (gotcha.github.io) выдал бы доверие
		// всему github.io. Родительский домен указывается явно.
		{"почта на родительском домене", alert.Channel{Kind: alert.ChannelEmail, Target: "oncall@corp.example"}, false},
		{"почта на публичном сервисе", alert.Channel{Kind: alert.ChannelEmail, Target: "someone@gmail.com"}, false},
		{"вебхук на поддомен инстанса", alert.Channel{Kind: alert.ChannelWebhook, Target: "https://hooks.gotcha.corp.example/x"}, true},
		{"вебхук наружу", alert.Channel{Kind: alert.ChannelWebhook, Target: "https://hooks.slack.com/x"}, false},
		{"вебхук во внутреннюю сеть", alert.Channel{Kind: alert.ChannelWebhook, Target: "http://10.1.2.3:9000/x"}, true},
		{"вебхук на localhost", alert.Channel{Kind: alert.ChannelWebhook, Target: "http://localhost:9000/x"}, true},
		{"вебхук в нероутируемую зону", alert.Channel{Kind: alert.ChannelWebhook, Target: "https://alerts.internal/x"}, true},
		{"telegram", alert.Channel{Kind: alert.ChannelTelegram, Target: "-1001234567890"}, false},
	}
	for _, c := range cases {
		if got := p.AllowsDetails(c.ch); got != c.want {
			t.Errorf("%s: AllowsDetails(%s %q) = %v, want %v", c.name, c.ch.Kind, c.ch.Target, got, c.want)
		}
	}
}

// TestDetailPolicyTrustedRecipients — почта и вебхуки организации живут не
// обязательно на домене инстанса; для этого и существует список.
func TestDetailPolicyTrustedRecipients(t *testing.T) {
	p := alert.NewDetailPolicy("https://gotcha.example", []string{"corp.example", "Ops.Example."}, false)

	allowed := []alert.Channel{
		{Kind: alert.ChannelEmail, Target: "oncall@corp.example"},
		{Kind: alert.ChannelEmail, Target: "a@team.corp.example"},
		{Kind: alert.ChannelWebhook, Target: "https://hooks.ops.example/x"},
	}
	for _, ch := range allowed {
		if !p.AllowsDetails(ch) {
			t.Errorf("AllowsDetails(%s %q) = false, want true", ch.Kind, ch.Target)
		}
	}
	// Суффикс совпадает по ГРАНИЦЕ метки, а не по строке: evilcorp.example не
	// поддомен corp.example, и подставить такой домен нельзя.
	denied := []alert.Channel{
		{Kind: alert.ChannelEmail, Target: "a@evilcorp.example"},
		{Kind: alert.ChannelWebhook, Target: "https://notcorp.example/x"},
	}
	for _, ch := range denied {
		if p.AllowsDetails(ch) {
			t.Errorf("AllowsDetails(%s %q) = true, want false (не поддомен)", ch.Kind, ch.Target)
		}
	}
}

// TestDetailPolicyAllowAll — глобальное разрешение оператора перекрывает всё,
// включая Telegram: он заявил законное основание для трансграничной передачи.
func TestDetailPolicyAllowAll(t *testing.T) {
	p := alert.NewDetailPolicy("https://gotcha.example", nil, true)
	for _, ch := range []alert.Channel{
		{Kind: alert.ChannelTelegram, Target: "-100500"},
		{Kind: alert.ChannelEmail, Target: "a@gmail.com"},
		{Kind: alert.ChannelWebhook, Target: "https://hooks.slack.com/x"},
	} {
		if !p.AllowsDetails(ch) {
			t.Errorf("AllowsDetails(%s) = false при allowAll", ch.Kind)
		}
	}
}

// TestDetailPolicyFailsClosed — что не разобралось или не опознано, деталей не
// получает. Нулевая политика не доверяет никому: забытое поле у нового
// нотифаера не должно означать «шлём всё».
func TestDetailPolicyFailsClosed(t *testing.T) {
	var zero alert.DetailPolicy
	if zero.AllowsDetails(alert.Channel{Kind: alert.ChannelEmail, Target: "a@corp.example"}) {
		t.Error("нулевая политика не должна разрешать детали")
	}

	p := alert.NewDetailPolicy("https://gotcha.corp.example", nil, false)
	for _, ch := range []alert.Channel{
		{Kind: alert.ChannelEmail, Target: "no-at-sign"},
		{Kind: alert.ChannelEmail, Target: ""},
		{Kind: alert.ChannelWebhook, Target: "not a url"},
		{Kind: alert.ChannelWebhook, Target: ""},
		{Kind: "sms", Target: "+70000000000"},
	} {
		if p.AllowsDetails(ch) {
			t.Errorf("AllowsDetails(%s %q) = true, want false", ch.Kind, ch.Target)
		}
	}
}

// TestDetailPolicyNormalizesHost — регистр, порт, корневая точка и IPv6 в
// скобках не должны менять решение: иначе доверенный получатель терял бы
// детали из-за формы записи адреса.
func TestDetailPolicyNormalizesHost(t *testing.T) {
	p := alert.NewDetailPolicy("https://GOTCHA.Corp.Example./", nil, false)
	for _, ch := range []alert.Channel{
		{Kind: alert.ChannelEmail, Target: "A@GOTCHA.CORP.EXAMPLE"},
		{Kind: alert.ChannelEmail, Target: "a@gotcha.corp.example."},
		{Kind: alert.ChannelWebhook, Target: "https://hooks.GOTCHA.corp.example:8443/x"},
		{Kind: alert.ChannelWebhook, Target: "http://[::1]:9000/x"},
	} {
		if !p.AllowsDetails(ch) {
			t.Errorf("AllowsDetails(%s %q) = false, want true", ch.Kind, ch.Target)
		}
	}
}

// TestDetailPolicyEmailLocalPartWithAt — домен берётся по ПОСЛЕДНЕМУ '@':
// локальная часть по RFC 5321 может содержать его в кавычках, и разбор по
// первому отдал бы за домен кусок локальной части — то есть чужой адрес мог бы
// притвориться доверенным.
func TestDetailPolicyEmailLocalPartWithAt(t *testing.T) {
	p := alert.NewDetailPolicy("https://gotcha.corp.example", nil, false)
	if p.AllowsDetails(alert.Channel{Kind: alert.ChannelEmail, Target: `"a@gotcha.corp.example"@gmail.com`}) {
		t.Error("домен должен браться по последнему '@'")
	}
}

// TestEvaluatorGatesEmailByRecipientDomain — сквозная проверка того, ради чего
// политика и переписана: почта решается по домену получателя.
//
// Ящик организации получает детали, ящик на публичном почтовом сервисе — нет.
// Раньше оба получали: гейт смотрел на транспорт, а email считался «своим» по
// определению, и текст ошибки с возможными ПДн уезжал на @gmail.com.
func TestEvaluatorGatesEmailByRecipientDomain(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()

	pid := newEvalProject(t, pool, "detgate")
	issueID := newEvalIssue(t, pool, pid, "fp-detgate")
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 30,
	}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	insideID, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true, Target: "oncall@corp.example",
	})
	if err != nil {
		t.Fatalf("CreateChannel inside: %v", err)
	}
	outsideID, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true, Target: "someone@gmail.com",
	})
	if err != nil {
		t.Fatalf("CreateChannel outside: %v", err)
	}

	e := &alert.Evaluator{
		Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", EmailEnabled: true,
		Details: alert.NewDetailPolicy("https://gotcha.example", []string{"corp.example"}, false),
	}
	e.OnIssue(ctx, alert.Event{
		ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue,
		Title: "boom", Culprit: "app.x", Level: "error", TimesSeen: 3,
	})

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(jobs))
	}
	for _, j := range jobs {
		_, hasTitle := j.Payload["title"]
		switch j.ChannelID {
		case insideID:
			if !hasTitle {
				t.Errorf("ящик организации не получил деталей: %+v", j.Payload)
			}
		case outsideID:
			if hasTitle {
				t.Errorf("ящик на публичном сервисе получил детали: %+v", j.Payload)
			}
		default:
			t.Errorf("неожиданный канал %d", j.ChannelID)
		}
	}
}

// TestDetailPolicyDefaultStandIsTrusted — типовой локальный стенд не должен
// требовать настройки: инстанс на localhost и почта на .local — заведомо своя
// инфраструктура. Тест держит этот случай явно, потому что именно на нём
// строгий дефолт заметили бы первым, если бы он оказался слишком строгим.
func TestDetailPolicyDefaultStandIsTrusted(t *testing.T) {
	p := alert.NewDetailPolicy("http://localhost:59080", nil, false)
	for _, ch := range []alert.Channel{
		{Kind: alert.ChannelEmail, Target: "demo@gotcha.local"},
		{Kind: alert.ChannelWebhook, Target: "http://localhost:59080/hook"},
	} {
		if !p.AllowsDetails(ch) {
			t.Errorf("AllowsDetails(%s %q) = false, want true", ch.Kind, ch.Target)
		}
	}
}
