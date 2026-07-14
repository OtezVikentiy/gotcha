package ingest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Репрезентативный payload sentry-php.
const phpEvent = `{
  "event_id": "9ec79c33ec9942ab8353589fcb2e04dc",
  "timestamp": 1752451200.123,
  "platform": "php",
  "level": "error",
  "environment": "production",
  "release": "app@2.1.0",
  "server_name": "web-3",
  "sdk": {"name": "sentry.php", "version": "4.10.0"},
  "user": {"id": "42", "ip_address": "192.0.2.10", "email": "u@example.com"},
  "tags": {"module": "billing"},
  "exception": {"values": [{
    "type": "RuntimeException",
    "value": "Payment failed for order 981",
    "stacktrace": {"frames": [
      {"function": "handle", "module": "Symfony\\HttpKernel", "in_app": false, "lineno": 80},
      {"function": "charge", "module": "App\\Billing", "in_app": true, "lineno": 42},
      {"function": "capture", "module": "App\\Billing\\Gateway", "in_app": true, "lineno": 17}
    ]}
  }]},
  "fingerprint": ["{{ default }}", "billing"]
}`

func TestParseEventPHP(t *testing.T) {
	pe, err := ParseEvent([]byte(phpEvent))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if pe.EventID != "9ec79c33-ec99-42ab-8353-589fcb2e04dc" {
		t.Errorf("EventID = %q, want canonical uuid", pe.EventID)
	}
	// float64 не хранит .123 точно — сравниваем с допуском 1ms.
	want := time.Unix(1752451200, 123_000_000).UTC()
	if d := pe.Timestamp.Sub(want); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("Timestamp = %v, want %v ±1ms", pe.Timestamp, want)
	}
	if pe.Level != "error" || pe.Environment != "production" || pe.Release != "app@2.1.0" {
		t.Errorf("meta: %+v", pe)
	}
	if pe.SDK != "sentry.php/4.10.0" {
		t.Errorf("SDK = %q", pe.SDK)
	}
	if pe.UserID != "42" || pe.UserIP != "192.0.2.10" || pe.UserEmail != "u@example.com" {
		t.Errorf("user: %+v", pe)
	}
	if pe.Tags["module"] != "billing" {
		t.Errorf("tags: %v", pe.Tags)
	}
	if len(pe.Exceptions) != 1 || pe.Exceptions[0].Type != "RuntimeException" {
		t.Fatalf("exceptions: %+v", pe.Exceptions)
	}
	fr := pe.Exceptions[0].Frames
	if len(fr) != 3 || !fr[1].InApp || fr[0].InApp {
		t.Errorf("frames: %+v", fr)
	}
	if pe.Title != "RuntimeException: Payment failed for order 981" {
		t.Errorf("Title = %q", pe.Title)
	}
	if pe.Culprit != "App\\Billing\\Gateway.capture" {
		t.Errorf("Culprit = %q", pe.Culprit)
	}
	if len(pe.Fingerprint) != 2 || pe.Fingerprint[0] != "{{ default }}" {
		t.Errorf("fingerprint: %v", pe.Fingerprint)
	}
	if pe.StacktraceJSON == "" || pe.ContextsJSON != "" {
		t.Errorf("json blobs: stack=%q contexts=%q", pe.StacktraceJSON, pe.ContextsJSON)
	}
}

func TestParseEventMessageOnly(t *testing.T) {
	// sentry-js captureMessage: message-объект, ISO-timestamp, тэги массивом.
	raw := `{
	  "event_id": "abcdefabcdefabcdefabcdefabcdefab",
	  "timestamp": "2026-07-14T10:00:00.5Z",
	  "message": {"formatted": "timeout for user 55"},
	  "tags": [["browser", "firefox"], ["page", "/checkout"]]
	}`
	pe, err := ParseEvent([]byte(raw))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if pe.Message != "timeout for user 55" {
		t.Errorf("Message = %q", pe.Message)
	}
	if pe.Level != "error" {
		t.Errorf("default level = %q", pe.Level)
	}
	if !pe.Timestamp.Equal(time.Date(2026, 7, 14, 10, 0, 0, 500_000_000, time.UTC)) {
		t.Errorf("Timestamp = %v", pe.Timestamp)
	}
	if pe.Tags["browser"] != "firefox" || pe.Tags["page"] != "/checkout" {
		t.Errorf("tags: %v", pe.Tags)
	}
	if pe.Title != "timeout for user 55" || pe.Culprit != "" {
		t.Errorf("title=%q culprit=%q", pe.Title, pe.Culprit)
	}
}

func TestParseEventDefaults(t *testing.T) {
	pe, err := ParseEvent([]byte(`{"message":"bare"}`))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if pe.EventID == "" || len(pe.EventID) != 36 {
		t.Errorf("EventID must be generated: %q", pe.EventID)
	}
	if pe.Timestamp.IsZero() {
		t.Error("Timestamp must default to now")
	}
}

func TestParseEventGarbage(t *testing.T) {
	if _, err := ParseEvent([]byte("{broken")); err == nil {
		t.Fatal("want error")
	}
}

func TestParseEventLevelWhitelist(t *testing.T) {
	pe, err := ParseEvent([]byte(`{"level":"custom","message":"x"}`))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if pe.Level != "error" {
		t.Errorf("Level = %q, want error (unknown level falls back)", pe.Level)
	}
}

func TestParseEventCapsUntrustedFields(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"environment": strings.Repeat("e", 500),
		"message":     strings.Repeat("m", 10000),
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	pe, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if n := len([]rune(pe.Environment)); n != 200 {
		t.Errorf("Environment len = %d, want 200", n)
	}
	if n := len([]rune(pe.Message)); n != 8192 {
		t.Errorf("Message len = %d, want 8192", n)
	}
}

func TestParseEventCapsCulprit(t *testing.T) {
	ev := map[string]any{
		"exception": map[string]any{
			"values": []map[string]any{{
				"type":  "Err",
				"value": "boom",
				"stacktrace": map[string]any{
					"frames": []map[string]any{
						{"module": strings.Repeat("M", 500), "function": "f", "in_app": true},
					},
				},
			}},
		},
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	pe, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if n := len([]rune(pe.Culprit)); n != 200 {
		t.Errorf("Culprit len = %d, want 200 (capped)", n)
	}
}

func TestParseEventCapsTagCount(t *testing.T) {
	tags := map[string]string{}
	for i := 0; i < 100; i++ {
		tags[fmt.Sprintf("k%03d", i)] = "v"
	}
	raw, err := json.Marshal(map[string]any{"tags": tags})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	// Парсим дважды для проверки детерминизма.
	pe1, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("ParseEvent (1st): %v", err)
	}
	pe2, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("ParseEvent (2nd): %v", err)
	}

	// Проверяем что ровно 64 тега пережили.
	if len(pe1.Tags) != 64 {
		t.Errorf("len(Tags) = %d, want 64 (100 offered)", len(pe1.Tags))
	}

	// Проверяем детерминизм: обе парсения дали одинаковые теги.
	if !reflect.DeepEqual(pe1.Tags, pe2.Tags) {
		t.Error("Tags are not deterministic: first and second parse differ")
	}

	// Проверяем что это первые 64 в отсортированном порядке (k000..k063).
	if _, ok := pe1.Tags["k000"]; !ok {
		t.Error("Tag k000 missing (should be first alphabetically)")
	}
	if _, ok := pe1.Tags["k063"]; !ok {
		t.Error("Tag k063 missing (should be 64th alphabetically)")
	}
	if _, ok := pe1.Tags["k064"]; ok {
		t.Error("Tag k064 present (should not be included, only first 64)")
	}
	if _, ok := pe1.Tags["k099"]; ok {
		t.Error("Tag k099 present (should not be included)")
	}
}

func TestParseEventCapsTagValue(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"tags": map[string]string{"k": strings.Repeat("v", 1000)},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	pe, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if n := len([]rune(pe.Tags["k"])); n != 256 {
		t.Errorf("tag value len = %d, want 256", n)
	}
}
