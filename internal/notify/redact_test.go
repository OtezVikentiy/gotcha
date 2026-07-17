package notify_test

import (
	"strings"
	"testing"

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

	out := notify.RedactExternalPayload(full)

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
	if out["channel_kind"] != "telegram" || out["target"] != "123" || out["secret"] != "tok" {
		t.Errorf("transport fields lost: %+v", out)
	}
	if out["subject"] != "[gotcha] new_issue" {
		t.Errorf("subject = %v, want route-only", out["subject"])
	}
	if out["body"] != "new_issue\n\nhttps://gotcha.example/issues/42" {
		t.Errorf("body = %v, want route-only", out["body"])
	}
}

// TestRedactExternalPayloadDoesNotMutateInput гарантирует, что исходный
// payload (уходящий в email/внутренние каналы) не портится.
func TestRedactExternalPayloadDoesNotMutateInput(t *testing.T) {
	full := map[string]any{
		"kind": "down", "title": "boom", "url": "u",
		"channel_kind": "telegram", "target": "t", "secret": "s",
	}
	_ = notify.RedactExternalPayload(full)
	if full["title"] != "boom" {
		t.Fatalf("input mutated: %+v", full)
	}
}
