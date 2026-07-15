package ingest

import (
	"errors"
	"strings"
	"testing"
)

func TestParseEnvelopeBasic(t *testing.T) {
	raw := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","sent_at":"2026-07-14T00:00:00Z"}
{"type":"event","length":25}
{"message":"hello world"}
{"type":"attachment","length":5}
hello
{"type":"event"}
{"message":"second"}
`
	env, err := ParseEnvelope(strings.NewReader(raw), 1<<20)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if env.EventID != "9ec79c33ec9942ab8353589fcb2e04dc" {
		t.Errorf("EventID = %q", env.EventID)
	}
	if len(env.Events) != 2 {
		t.Fatalf("events = %d, want 2 (attachment skipped)", len(env.Events))
	}
	if !strings.Contains(string(env.Events[0]), "hello world") ||
		!strings.Contains(string(env.Events[1]), "second") {
		t.Errorf("payloads: %q, %q", env.Events[0], env.Events[1])
	}
}

// TestParseEnvelopeMixedItems: event и transaction в одном envelope'е
// раскладываются по разным спискам, прочие типы по-прежнему пропускаются.
func TestParseEnvelopeMixedItems(t *testing.T) {
	raw := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"message":"boom"}
{"type":"session"}
{"sid":"x"}
{"type":"transaction"}
{"transaction":"GET /x"}
`
	env, err := ParseEnvelope(strings.NewReader(raw), 1<<20)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Events) != 1 || !strings.Contains(string(env.Events[0]), "boom") {
		t.Errorf("events = %q", env.Events)
	}
	if len(env.Transactions) != 1 || !strings.Contains(string(env.Transactions[0]), "GET /x") {
		t.Errorf("transactions = %q", env.Transactions)
	}
}

func TestParseEnvelopeNoTrailingNewline(t *testing.T) {
	raw := "{}\n{\"type\":\"event\"}\n{\"message\":\"x\"}"
	env, err := ParseEnvelope(strings.NewReader(raw), 1<<20)
	if err != nil || len(env.Events) != 1 {
		t.Fatalf("events=%d err=%v", len(env.Events), err)
	}
}

func TestParseEnvelopeTooLargeItem(t *testing.T) {
	raw := "{}\n{\"type\":\"event\",\"length\":100}\n" + strings.Repeat("x", 100)
	if _, err := ParseEnvelope(strings.NewReader(raw), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestParseEnvelopeGarbage(t *testing.T) {
	if _, err := ParseEnvelope(strings.NewReader("not json at all"), 1<<20); err == nil {
		t.Fatal("want error for garbage input")
	}
}

func TestParseEnvelopeNegativeLength(t *testing.T) {
	raw := "{}\n{\"type\":\"event\",\"length\":-5}\nxxxxx\n"
	_, err := ParseEnvelope(strings.NewReader(raw), 1<<20)
	if err == nil {
		t.Fatal("want error for negative length, got nil (or panic)")
	}
	if errors.Is(err, ErrTooLarge) {
		t.Fatalf("negative length is malformed, not too-large: %v", err)
	}
}

func TestParseEnvelopeHeaderOnlyNoNewline(t *testing.T) {
	env, err := ParseEnvelope(strings.NewReader(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}`), 1<<20)
	if err != nil {
		t.Fatalf("single-line envelope without trailing newline: %v", err)
	}
	if env.EventID != "9ec79c33ec9942ab8353589fcb2e04dc" || len(env.Events) != 0 {
		t.Fatalf("env = %+v", env)
	}
}

func TestParseEnvelopeUnboundedLineCapped(t *testing.T) {
	raw := "{}\n{\"type\":\"event\"}\n" + strings.Repeat("a", 2048) + "\n"
	if _, err := ParseEnvelope(strings.NewReader(raw), 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestParseEnvelopeGarbageWithNewline(t *testing.T) {
	// Мусор с \n: ошибка должна прийти из JSON-валидации, не из EOF.
	if _, err := ParseEnvelope(strings.NewReader("not json at all\n"), 1<<20); err == nil {
		t.Fatal("want error for garbage header")
	}
}
