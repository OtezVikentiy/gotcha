package notify_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

func TestRedactExternalPayloadStripsDetails(t *testing.T) {
	full := map[string]any{
		"kind":          notify.KindNewIssue,
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

	for _, k := range []string{"title", "culprit", "level", "target_name", "monitor_name", "function", "cause"} {
		if _, ok := out[k]; ok {
			t.Errorf("redacted payload leaks %q: %+v", k, out)
		}
	}
	if subj, _ := out["subject"].(string); strings.Contains(subj, "boom") || strings.Contains(subj, "SELECT") {
		t.Errorf("subject leaks details: %q", subj)
	}
	if body, _ := out["body"].(string); strings.Contains(body, "boom") || strings.Contains(body, "SELECT") {
		t.Errorf("body leaks details: %q", body)
	}
	if out["url"] != "https://gotcha.example/issues/42" {
		t.Errorf("url lost: %+v", out)
	}
	if out["kind"] != notify.KindNewIssue {
		t.Errorf("kind lost: %+v", out)
	}
	if out["channel_kind"] != "telegram" || out["target"] != "123" {
		t.Errorf("transport fields lost: %+v", out)
	}
	// Секрет вырезается наравне с прочим — воркер резолвит его по channel_id,
	// из payload он не нужен.
	if _, ok := out["secret"]; ok {
		t.Errorf("secret не должен переживать редакцию: %+v", out)
	}
	if out["subject"] != "[Gotcha] New issue" {
		t.Errorf("subject = %v, want humanized route-only", out["subject"])
	}
	if out["body"] != "New issue\n\nhttps://gotcha.example/issues/42" {
		t.Errorf("body = %v, want humanized route-only", out["body"])
	}
}

func TestRedactExternalPayloadLocalizedLabel(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	out := notify.RedactExternalPayload(ctx, map[string]any{
		"kind": notify.KindDown, "url": "https://gotcha.example/monitors/1",
	})
	if out["subject"] != "[Gotcha] Монитор недоступен" {
		t.Errorf("subject = %v, want russian label", out["subject"])
	}
}

func TestRedactExternalPayloadUnknownKind(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	// Нарочно вне реестра — тест проверяет поведение на незнакомом виде.
	out := notify.RedactExternalPayload(ctx, map[string]any{
		"kind": "mystery_kind", "url": "u",
	})
	if out["subject"] != "[Gotcha] mystery_kind" {
		t.Errorf("subject = %v, want raw kind fallback", out["subject"])
	}
}

func TestRedactExternalPayloadShortensURLWhenAsked(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	out := notify.RedactExternalPayload(ctx, map[string]any{
		"kind":         notify.KindHostAlertOpen,
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

func TestRedactExternalPayloadKeepsURLWithoutDirective(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	out := notify.RedactExternalPayload(ctx, map[string]any{
		"kind": notify.KindMetricAlertOpen, "url": "https://gotcha.example/metrics/1",
	})
	if out["url"] != "https://gotcha.example/metrics/1" {
		t.Errorf("url = %v, want исходную ссылку", out["url"])
	}
	if body, _ := out["body"].(string); !strings.Contains(body, "https://gotcha.example/metrics/1") {
		t.Errorf("body потерял ссылку: %q", body)
	}
}

func TestRedactExternalPayloadDoesNotMutateInput(t *testing.T) {
	full := map[string]any{
		"kind": notify.KindDown, "title": "boom", "url": "u",
		"channel_kind": "telegram", "target": "t", "secret": "s",
	}
	_ = notify.RedactExternalPayload(context.Background(), full)
	if full["title"] != "boom" {
		t.Fatalf("input mutated: %+v", full)
	}
}
