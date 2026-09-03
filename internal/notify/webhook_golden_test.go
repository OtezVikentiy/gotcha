package notify_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// Значения фикстуры issue-алерта — те же, что и в internal/docs/{ru,en}/
// alerts.md, раздел «Формат тела вебхука»: пример JSON в доках — буквально
// содержимое золотых файлов ниже, так что менять одно без другого нельзя.
const (
	fixtureProjectID   = 7
	fixtureIssueID     = 42
	fixtureTitle       = "TypeError: cannot read properties of undefined"
	fixtureCulprit     = "checkoutHandler"
	fixtureLevel       = "error"
	fixtureTimesSeen   = 3
	fixtureURL         = "https://gotcha.example/issues/42"
	fixtureProjectName = "Storefront"
)

// capturingOutbox реализует escalation.Enqueuer, запоминая payload вместо
// похода в Postgres — так тест гоняет РЕАЛЬНЫЙ escalation.Dispatch (резолв
// имени проекта, сборку Extra, гейт AllowsDetails/RedactExternalPayload),
// не переписывая эту логику фикстурой, набранной руками.
type capturingOutbox struct {
	payload map[string]any
}

func (o *capturingOutbox) Enqueue(_ context.Context, _ int64, payload map[string]any) error {
	o.payload = payload
	return nil
}

// fixedProjectNamer — детерминированная замена escalation.ProjectNamer:
// реальный OrgProjectNamer ходит в БД, а имени проекта здесь достаточно
// быть постоянным.
type fixedProjectNamer struct{ name string }

func (f fixedProjectNamer) ProjectName(_ context.Context, _ int64) (string, error) {
	return f.name, nil
}

// dispatchIssueAlertFixture строит payload issue-алерта (new_issue) ровно
// так, как это делает alert.Evaluator.OnIssue перед вызовом
// escalation.Dispatch (см. internal/alert/evaluator.go) — те же ключи Extra
// (issue_id/title/culprit/level/times_seen), тот же формат subject/body
// через i18n-ключи notify.issue.subject/notify.issue.body. Evaluator сюда
// не зовётся напрямую: он тянет БД (правило, троттлинг, каналы проекта), а
// заморозке подлежит именно то, что происходит ПОСЛЕ — единая точка сборки
// payload в Dispatch.
//
// Локаль фикстуры — en: контракт вебхука адресован разработчику получателя,
// не оператору инстанса, и пример в доках должен читаться независимо от
// GOTCHA_LOCALE конкретной инсталляции (по умолчанию — ru).
func dispatchIssueAlertFixture(t *testing.T, allowsDetails bool) map[string]any {
	t.Helper()
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})

	out := &capturingOutbox{}
	deps := escalation.DispatchDeps{
		Outbox:       out,
		EmailEnabled: true,
		Projects:     fixedProjectNamer{fixtureProjectName},
		LogTag:       "test",
	}
	in := escalation.DispatchInput{
		ProjectID: fixtureProjectID,
		Kind:      "new_issue",
		Subject: i18n.Tf(ctx, "notify.issue.subject",
			"kind", i18n.T(ctx, "notify.issue.kind.new_issue"), "title", fixtureTitle),
		Body: i18n.Tf(ctx, "notify.issue.body",
			"title", fixtureTitle, "culprit", fixtureCulprit, "level", fixtureLevel,
			"count", "3", "url", fixtureURL),
		URL: fixtureURL,
		Extra: map[string]any{
			"issue_id":   int64(fixtureIssueID),
			"title":      fixtureTitle,
			"culprit":    fixtureCulprit,
			"level":      fixtureLevel,
			"times_seen": int64(fixtureTimesSeen),
		},
		Channels: []escalation.DispatchChannel{
			{ID: 1, Kind: "webhook", Target: "https://receiver.example/hook",
				Deliverable: true, AllowsDetails: allowsDetails},
		},
	}
	if _, err := escalation.Dispatch(ctx, deps, in); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if out.payload == nil {
		t.Fatal("dispatch enqueued nothing — fixture channel not deliverable?")
	}
	return out.payload
}

// TestWebhookBodyGolden замораживает тело исходящего вебхука issue-алерта
// в обоих режимах AllowsDetails (задача 13, E3): проходит РЕАЛЬНЫЙ путь
// escalation.Dispatch -> notify.WebhookSender.Send -> HTTP-запрос,
// перехватывает то, что реально ушло на приёмник (httptest.Server), и
// сравнивает с золотым файлом. Пример JSON в internal/docs/{ru,en}/
// alerts.md — содержимое этих же золотых файлов (см. TestWebhookBodyDocsMatchGolden
// в internal/guards).
func TestWebhookBodyGolden(t *testing.T) {
	cases := []struct {
		name          string
		allowsDetails bool
		golden        string
	}{
		{"details", true, "testdata/webhook_body_details.json"},
		{"redacted", false, "testdata/webhook_body_redacted.json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := dispatchIssueAlertFixture(t, tc.allowsDetails)

			var gotBody []byte
			var gotSig string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("reading request body: %v", err)
				}
				gotBody = b
				gotSig = r.Header.Get("X-Gotcha-Signature")
				if ct := r.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
				if r.Method != http.MethodPost {
					t.Errorf("method = %q, want POST", r.Method)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			const secret = "test-signing-secret"
			sender := &notify.WebhookSender{Client: srv.Client()}
			target := notify.Target{Kind: "webhook", Target: srv.URL, Secret: secret}
			if err := sender.Send(context.Background(), target, payload); err != nil {
				t.Fatalf("send: %v", err)
			}
			if gotBody == nil {
				t.Fatal("receiver got no request body")
			}

			wantBody, err := os.ReadFile(tc.golden)
			if err != nil {
				t.Fatalf("reading golden file: %v", err)
			}
			gotCanon := canonicalJSON(t, gotBody)
			wantCanon := canonicalJSON(t, wantBody)
			if gotCanon != wantCanon {
				t.Errorf("webhook body != golden %s\n--- got ---\n%s\n--- want ---\n%s",
					tc.golden, gotCanon, wantCanon)
			}

			// Подпись — самосогласованность: HMAC-SHA256(secret) по РЕАЛЬНО
			// отправленным байтам обязан совпасть с заголовком, ровно как
			// описано в доках («Формат тела вебхука»).
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(gotBody)
			wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			if gotSig != wantSig {
				t.Errorf("X-Gotcha-Signature = %q, want %q", gotSig, wantSig)
			}

			// Контракт transportFields (webhook.go): channel_kind/target/secret
			// никогда не попадают в тело — это внутренние поля воркера, а
			// secret в теле обесценил бы саму подпись.
			for _, k := range []string{"channel_kind", "target", "secret"} {
				needle := []byte(`"` + k + `"`)
				if bytes.Contains(gotBody, needle) {
					t.Errorf("webhook body leaks transport field %q: %s", k, gotBody)
				}
			}
		})
	}
}

// TestWebhookSendStripsSecretEvenIfProducerAddsIt закрывает дыру в
// TestWebhookBodyGolden: сегодня НИ ОДИН продюсер (escalation.Dispatch,
// alert/digest.go, trace/notify.go, web/alerts.go) не кладёт "secret" в
// payload очереди — секрет достаётся отдельно через notify.SecretResolver
// в момент отправки (см. комментарий transportFields в webhook.go), так что
// ассерт «в теле нет transport-полей» в TestWebhookBodyGolden ни разу не
// видит реального ключа "secret" и не может покраснеть при его утечке.
// Контракт из брифа задачи 13 («поле transportFields в теле отсутствует»)
// — про ВСЕ ТРИ ключа, а не только про те, что реально прислал сегодняшний
// producer: он обязан пережить гипотетическую будущую утечку secret в
// payload, а не полагаться на то, что её сегодня нет. Здесь "secret"
// дописывается в уже собранный Dispatch'ем payload вручную — так тест
// проверяет именно вырезание в WebhookSender.Send, а не то, кладёт ли его
// туда Dispatch.
func TestWebhookSendStripsSecretEvenIfProducerAddsIt(t *testing.T) {
	payload := dispatchIssueAlertFixture(t, true)

	if _, ok := payload["channel_kind"]; !ok {
		t.Fatal("fixture payload has no channel_kind — sanity check broken")
	}
	if _, ok := payload["target"]; !ok {
		t.Fatal("fixture payload has no target — sanity check broken")
	}
	if _, ok := payload["secret"]; ok {
		t.Fatal("fixture payload already has secret — adjust the injection below")
	}
	payload["secret"] = "leaked-signing-secret"

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &notify.WebhookSender{Client: srv.Client()}
	target := notify.Target{Kind: "webhook", Target: srv.URL, Secret: "test-signing-secret"}
	if err := sender.Send(context.Background(), target, payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotBody == nil {
		t.Fatal("receiver got no request body")
	}

	for _, k := range []string{"channel_kind", "target", "secret"} {
		needle := []byte(`"` + k + `"`)
		if bytes.Contains(gotBody, needle) {
			t.Errorf("webhook body leaks transport field %q: %s", k, gotBody)
		}
	}

	// Вырезание "secret" не должно менять само тело: сравнение с тем же
	// золотым файлом, что и режим "с деталями" в TestWebhookBodyGolden,
	// обязано совпасть.
	wantBody, err := os.ReadFile("testdata/webhook_body_details.json")
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got, want := canonicalJSON(t, gotBody), canonicalJSON(t, wantBody); got != want {
		t.Errorf("webhook body with injected secret != golden testdata/webhook_body_details.json\n--- got ---\n%s\n--- want ---\n%s",
			got, want)
	}
}

// canonicalJSON перепечатывает JSON с отсортированными ключами и отступами
// (json.Marshal сам сортирует ключи map[string]any) — так расхождение
// золотого сравнения печатает читаемую дельту, а не байт-в-байт то, что
// зависит от порядка полей в исходном marshal'е.
func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(b)
}
