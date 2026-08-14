package notify_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// TestRedactExternalPayloadStripsDetails проверяет, что обезличивание
// выкидывает текст ошибки/детали и оставляет только маршрутный минимум.
func TestRedactExternalPayloadStripsDetails(t *testing.T) {
	full := map[string]any{
		"kind":          "new_issue",
		"project_id":    int64(7),
		"issue_id":      int64(42),
		"perf_issue_id": int64(42),
		"title":         "boom",
		"culprit":       "SELECT * FROM users WHERE email = 'a@b.c'",
		"level":         "error",
		"target_name":   "GET /api/users",
		"monitor_name":  "internal-billing-db",
		"function":      "secretFn",
		"cause":         "connection refused",
		"times_seen":    int64(3),
		"count":         int64(3),
		"url":           "https://gotcha.example/issues/42",
		"subject":       "[gotcha] new_issue: boom",
		"body":          "boom\n\nCulprit: SELECT * FROM users",
		"channel_kind":  "telegram",
		"target":        "123",
		"secret":        "tok",
	}

	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	out := notify.RedactExternalPayload(ctx, full)

	// Чувствительные поля должны исчезнуть.
	for _, k := range []string{"title", "culprit", "level", "target_name", "monitor_name", "function", "cause"} {
		if _, ok := out[k]; ok {
			t.Errorf("redacted payload leaks %q: %+v", k, out)
		}
	}
	// subject/body не должны нести текст ошибки.
	if subj, _ := out["subject"].(string); strings.Contains(subj, "boom") || strings.Contains(subj, "SELECT") {
		t.Errorf("subject leaks details: %q", subj)
	}
	if body, _ := out["body"].(string); strings.Contains(body, "boom") || strings.Contains(body, "SELECT") {
		t.Errorf("body leaks details: %q", body)
	}
	// Маршрутный минимум остаётся.
	if out["url"] != "https://gotcha.example/issues/42" {
		t.Errorf("url lost: %+v", out)
	}
	if out["kind"] != "new_issue" {
		t.Errorf("kind lost: %+v", out)
	}
	if out["channel_kind"] != "telegram" || out["target"] != "123" {
		t.Errorf("transport fields lost: %+v", out)
	}
	// Секрет вырезается наравне с прочим: воркеру он из payload больше не
	// нужен (резолвит по channel_id), а в белом списке был только ради него.
	if _, ok := out["secret"]; ok {
		t.Errorf("secret не должен переживать редакцию: %+v", out)
	}
	// Тема/тело — человекочитаемая подпись вида на языке инстанса и ссылка,
	// в стиле остальных тем («[Gotcha] …»), а не сырой enum (QA MINOR-4).
	if out["subject"] != "[Gotcha] New issue" {
		t.Errorf("subject = %v, want humanized route-only", out["subject"])
	}
	if out["body"] != "New issue\n\nhttps://gotcha.example/issues/42" {
		t.Errorf("body = %v, want humanized route-only", out["body"])
	}
}

// TestRedactExternalPayloadLocalizedLabel: подпись вида берётся из каталога
// локали инстанса (GOTCHA_LOCALE), как и остальные тексты уведомлений.
func TestRedactExternalPayloadLocalizedLabel(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	out := notify.RedactExternalPayload(ctx, map[string]any{
		"kind": "down", "url": "https://gotcha.example/monitors/1",
	})
	if out["subject"] != "[Gotcha] Монитор недоступен" {
		t.Errorf("subject = %v, want russian label", out["subject"])
	}
}

// TestRedactExternalPayloadUnknownKind: незнакомый вид уходит сырым enum'ом,
// а не пустой строкой — та же честность, что у issueAlertKindLabel.
func TestRedactExternalPayloadUnknownKind(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	out := notify.RedactExternalPayload(ctx, map[string]any{
		"kind": "mystery_kind", "url": "u",
	})
	if out["subject"] != "[Gotcha] mystery_kind" {
		t.Errorf("subject = %v, want raw kind fallback", out["subject"])
	}
}

// TestRedactExternalPayloadShortensURLWhenAsked: нотифаер, у которого деталь
// несёт сам адрес карточки (хосты — /projects/{id}/hosts/{имя}), кладёт в
// payload "url_redacted"; редакция подставляет его и в url, и в тело. Само
// поле — директива, а не вывод: наружу отдельной строкой оно не идёт.
func TestRedactExternalPayloadShortensURLWhenAsked(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	out := notify.RedactExternalPayload(ctx, map[string]any{
		"kind":         "host_alert_open",
		"url":          "https://gotcha.example/projects/7/hosts/web-01",
		"url_redacted": "https://gotcha.example/projects/7/hosts",
	})
	if out["url"] != "https://gotcha.example/projects/7/hosts" {
		t.Errorf("url = %v, want сокращённую ссылку", out["url"])
	}
	if body, _ := out["body"].(string); strings.Contains(body, "web-01") {
		t.Errorf("body несёт имя хоста внутри ссылки: %q", body)
	}
	if _, ok := out["url_redacted"]; ok {
		t.Errorf("url_redacted не должен переживать редакцию отдельным полем: %+v", out)
	}
}

// TestRedactExternalPayloadKeepsURLWithoutDirective: без url_redacted ссылка
// остаётся прежней — поведение остальных нотифаеров не меняется.
func TestRedactExternalPayloadKeepsURLWithoutDirective(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	out := notify.RedactExternalPayload(ctx, map[string]any{
		"kind": "metric_alert_open", "url": "https://gotcha.example/metrics/1",
	})
	if out["url"] != "https://gotcha.example/metrics/1" {
		t.Errorf("url = %v, want исходную ссылку", out["url"])
	}
	if body, _ := out["body"].(string); !strings.Contains(body, "https://gotcha.example/metrics/1") {
		t.Errorf("body потерял ссылку: %q", body)
	}
}

// TestRedactExternalPayloadDoesNotMutateInput гарантирует, что исходный
// payload (уходящий в email/внутренние каналы) не портится.
func TestRedactExternalPayloadDoesNotMutateInput(t *testing.T) {
	full := map[string]any{
		"kind": "down", "title": "boom", "url": "u",
		"channel_kind": "telegram", "target": "t", "secret": "s",
	}
	_ = notify.RedactExternalPayload(context.Background(), full)
	if full["title"] != "boom" {
		t.Fatalf("input mutated: %+v", full)
	}
}
