package ingest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Репрезентативный payload sentry-php. timestamp относительный: абсолютную дату
// в фикстуре держать нельзя — parseTimestamp подтягивает всё, что вне окна
// хранения (см. timestamp.go), к его границе, и вшитая константа однажды сама по
// себе вывалилась бы за окно.
func phpEvent(ts time.Time) string {
	return fmt.Sprintf(`{
  "event_id": "9ec79c33ec9942ab8353589fcb2e04dc",
  "timestamp": %.3f,
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
}`, float64(ts.UnixNano())/1e9)
}

func TestParseEventPHP(t *testing.T) {
	want := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	pe, err := ParseEvent([]byte(phpEvent(want)))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if pe.EventID != "9ec79c33-ec99-42ab-8353-589fcb2e04dc" {
		t.Errorf("EventID = %q, want canonical uuid", pe.EventID)
	}
	// float64 не хранит доли секунды точно — сравниваем с допуском 1ms.
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
	want := time.Now().UTC().Add(-2 * time.Hour).Truncate(100 * time.Millisecond)
	raw := fmt.Sprintf(`{
	  "event_id": "abcdefabcdefabcdefabcdefabcdefab",
	  "timestamp": %q,
	  "message": {"formatted": "timeout for user 55"},
	  "tags": [["browser", "firefox"], ["page", "/checkout"]]
	}`, want.Format(time.RFC3339Nano))
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
	if !pe.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", pe.Timestamp, want)
	}
	if pe.Tags["browser"] != "firefox" || pe.Tags["page"] != "/checkout" {
		t.Errorf("tags: %v", pe.Tags)
	}
	if pe.Title != "timeout for user 55" || pe.Culprit != "" {
		t.Errorf("title=%q culprit=%q", pe.Title, pe.Culprit)
	}
}

// TestParseEventClampsTimestampToWindow: events партиционирована по
// toYYYYMM(timestamp), и пачка событий с timestamp'ами из сотни разных месяцев
// заклинила бы вставку целиком («Too many partitions for single INSERT block»),
// поэтому timestamp вне окна [now-90d, now+1d] подтягивается к границе. Само
// событие при этом НЕ теряется: ошибка со стектрейсом ценнее её timestamp'а.
func TestParseEventClampsTimestampToWindow(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		raw  string
		want time.Time
	}{
		{"prehistoric unix", `{"message":"x","timestamp":946684800}`, now.Add(-maxTimestampAge)},
		{"older than TTL", fmt.Sprintf(`{"message":"x","timestamp":%q}`,
			now.Add(-200*24*time.Hour).Format(time.RFC3339Nano)), now.Add(-maxTimestampAge)},
		{"far future", fmt.Sprintf(`{"message":"x","timestamp":%d}`,
			now.Add(365*24*time.Hour).Unix()), now.Add(maxTimestampFuture)},
	}
	for _, tc := range tests {
		pe, err := ParseEvent([]byte(tc.raw))
		if err != nil {
			t.Fatalf("%s: ParseEvent: %v", tc.name, err)
		}
		if d := pe.Timestamp.Sub(tc.want); d < -time.Minute || d > time.Minute {
			t.Errorf("%s: Timestamp = %v, want ~%v (clamped to the retention window)",
				tc.name, pe.Timestamp, tc.want)
		}
	}

	// Внутри окна timestamp остаётся нетронутым.
	inWindow := now.Add(-24 * time.Hour).Truncate(time.Millisecond)
	pe, err := ParseEvent([]byte(fmt.Sprintf(`{"message":"x","timestamp":%q}`,
		inWindow.Format(time.RFC3339Nano))))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if !pe.Timestamp.Equal(inWindow) {
		t.Errorf("in-window Timestamp = %v, want %v (untouched)", pe.Timestamp, inWindow)
	}
}

// TestParseEventLowercasesTraceIDs: contexts.trace.trace_id/span_id хранятся
// каноническим hex'ом в нижнем регистре — иначе events не сджойнятся с spans по
// trace_id (регистр выбирает тот, кто кодирует id; в OTLP он едет сырыми байтами).
func TestParseEventLowercasesTraceIDs(t *testing.T) {
	pe, err := ParseEvent([]byte(`{"message":"x","contexts":{"trace":{
		"trace_id":"4BF92F3577B34DA6A3CE929D0E0E4736","span_id":"00F067AA0BA902B7"}}}`))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if pe.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || pe.SpanID != "00f067aa0ba902b7" {
		t.Errorf("trace ids = %q/%q, want lowercase hex", pe.TraceID, pe.SpanID)
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

func TestParseEventCapsUserFields(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"user": map[string]any{
			"id":         strings.Repeat("i", 500),
			"ip_address": strings.Repeat("p", 500),
			"email":      strings.Repeat("e", 500),
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	pe, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if n := len([]rune(pe.UserID)); n != 200 {
		t.Errorf("UserID len = %d, want 200", n)
	}
	if n := len([]rune(pe.UserIP)); n != 200 {
		t.Errorf("UserIP len = %d, want 200", n)
	}
	if n := len([]rune(pe.UserEmail)); n != 200 {
		t.Errorf("UserEmail len = %d, want 200", n)
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
