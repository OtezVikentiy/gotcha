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
	env, err := ParseEnvelope(strings.NewReader(raw), 1<<20, nil)
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
{"type":"profile"}
{"platform":"go"}
`
	env, err := ParseEnvelope(strings.NewReader(raw), 1<<20, nil)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Events) != 1 || !strings.Contains(string(env.Events[0]), "boom") {
		t.Errorf("events = %q", env.Events)
	}
	if len(env.Transactions) != 1 || !strings.Contains(string(env.Transactions[0]), "GET /x") {
		t.Errorf("transactions = %q", env.Transactions)
	}
	if len(env.Profiles) != 1 || !strings.Contains(string(env.Profiles[0]), "\"platform\":\"go\"") {
		t.Errorf("profiles = %q", env.Profiles)
	}
}

func TestParseEnvelopeNoTrailingNewline(t *testing.T) {
	raw := "{}\n{\"type\":\"event\"}\n{\"message\":\"x\"}"
	env, err := ParseEnvelope(strings.NewReader(raw), 1<<20, nil)
	if err != nil || len(env.Events) != 1 {
		t.Fatalf("events=%d err=%v", len(env.Events), err)
	}
}

func TestParseEnvelopeTooLargeItem(t *testing.T) {
	raw := "{}\n{\"type\":\"event\",\"length\":100}\n" + strings.Repeat("x", 100)
	if _, err := ParseEnvelope(strings.NewReader(raw), 10, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestParseEnvelopeGarbage(t *testing.T) {
	if _, err := ParseEnvelope(strings.NewReader("not json at all"), 1<<20, nil); err == nil {
		t.Fatal("want error for garbage input")
	}
}

func TestParseEnvelopeNegativeLength(t *testing.T) {
	raw := "{}\n{\"type\":\"event\",\"length\":-5}\nxxxxx\n"
	_, err := ParseEnvelope(strings.NewReader(raw), 1<<20, nil)
	if err == nil {
		t.Fatal("want error for negative length, got nil (or panic)")
	}
	if errors.Is(err, ErrTooLarge) {
		t.Fatalf("negative length is malformed, not too-large: %v", err)
	}
}

func TestParseEnvelopeHeaderOnlyNoNewline(t *testing.T) {
	env, err := ParseEnvelope(strings.NewReader(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}`), 1<<20, nil)
	if err != nil {
		t.Fatalf("single-line envelope without trailing newline: %v", err)
	}
	if env.EventID != "9ec79c33ec9942ab8353589fcb2e04dc" || len(env.Events) != 0 {
		t.Fatalf("env = %+v", env)
	}
}

func TestParseEnvelopeUnboundedLineCapped(t *testing.T) {
	raw := "{}\n{\"type\":\"event\"}\n" + strings.Repeat("a", 2048) + "\n"
	if _, err := ParseEnvelope(strings.NewReader(raw), 1024, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestParseEnvelopeGarbageWithNewline(t *testing.T) {
	// Мусор с \n: ошибка должна прийти из JSON-валидации, не из EOF.
	if _, err := ParseEnvelope(strings.NewReader("not json at all\n"), 1<<20, nil); err == nil {
		t.Fatal("want error for garbage header")
	}
}

// TestParseEnvelopeItemLimit: сверх maxEnvelopeItems item'ы отбрасываются
// (защита от амплификации), принятые — сохраняются, лишние учтены в Dropped.
func TestParseEnvelopeItemLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("{}\n")
	const extra = 5
	for i := 0; i < maxEnvelopeItems+extra; i++ {
		b.WriteString("{\"type\":\"event\"}\n{\"message\":\"x\"}\n")
	}
	env, err := ParseEnvelope(strings.NewReader(b.String()), 1<<20, nil)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Events) != maxEnvelopeItems {
		t.Fatalf("kept events = %d, want %d", len(env.Events), maxEnvelopeItems)
	}
	if env.Dropped != extra {
		t.Fatalf("Dropped = %d, want %d", env.Dropped, extra)
	}
}

// TestParseEnvelopeUnknownTypesNotCounted: неизвестные типы не считаются в лимит
// (амплификацию не создают — они и так игнорируются).
func TestParseEnvelopeUnknownTypesNotCounted(t *testing.T) {
	var b strings.Builder
	b.WriteString("{}\n")
	for i := 0; i < maxEnvelopeItems; i++ {
		b.WriteString("{\"type\":\"session\"}\n{\"x\":1}\n") // игнорируемый тип
	}
	b.WriteString("{\"type\":\"event\"}\n{\"message\":\"kept\"}\n")
	env, err := ParseEnvelope(strings.NewReader(b.String()), 1<<20, nil)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if len(env.Events) != 1 || env.Dropped != 0 {
		t.Fatalf("events=%d dropped=%d, want 1/0 (unknown types uncounted)", len(env.Events), env.Dropped)
	}
}

// TestParseEnvelopeScopeFilter — item, чей сигнал ключу не разрешён,
// отбрасывается ПОШТУЧНО, остальные принимаются: браузерный envelope не
// должен терять события из-за одного лишнего profile-item'а.
func TestParseEnvelopeScopeFilter(t *testing.T) {
	raw := strings.Join([]string{
		`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}`,
		`{"type":"event"}`, `{"message":"e"}`,
		`{"type":"profile"}`, `{"profile":"p"}`,
		`{"type":"transaction"}`, `{"tx":"t"}`,
		"",
	}, "\n")
	allow := func(s IngestSignal) bool { return s != SignalProfile }
	env, err := ParseEnvelope(strings.NewReader(raw), 1<<20, allow)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(env.Events) != 1 || len(env.Transactions) != 1 {
		t.Fatalf("события/транзакции потеряны: %d/%d", len(env.Events), len(env.Transactions))
	}
	if len(env.Profiles) != 0 {
		t.Fatalf("профиль принят, хотя запрещён: %d", len(env.Profiles))
	}
	if env.ScopeRejected[SignalProfile] != 1 {
		t.Fatalf("ScopeRejected[profile] = %d, ожидалась 1", env.ScopeRejected[SignalProfile])
	}
	if len(env.ScopeRejected) != 1 {
		t.Fatalf("посчитаны лишние сигналы: %v", env.ScopeRejected)
	}
}

// TestParseEnvelopeNilAllow — nil-предикат означает «всё разрешено»:
// существующие вызовы разбора без скоупа поведения не меняют.
func TestParseEnvelopeNilAllow(t *testing.T) {
	raw := strings.Join([]string{
		`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}`,
		`{"type":"event"}`, `{"message":"e"}`,
		`{"type":"profile"}`, `{"profile":"p"}`,
		`{"type":"transaction"}`, `{"tx":"t"}`,
		"",
	}, "\n")
	env, err := ParseEnvelope(strings.NewReader(raw), 1<<20, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(env.Events) != 1 || len(env.Transactions) != 1 || len(env.Profiles) != 1 {
		t.Fatalf("item'ы потеряны: events=%d tx=%d profiles=%d",
			len(env.Events), len(env.Transactions), len(env.Profiles))
	}
	if len(env.ScopeRejected) != 0 {
		t.Fatalf("ScopeRejected не пуст при nil-предикате: %v", env.ScopeRejected)
	}
}
