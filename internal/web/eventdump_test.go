package web

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

func sampleEvent() event.Stored {
	return event.Stored{
		ID:             "11111111-1111-1111-1111-111111111111",
		Timestamp:      time.Date(2026, 8, 12, 14, 3, 11, 0, time.UTC),
		Level:          "error",
		ExceptionType:  "TypeError",
		ExceptionValue: "cannot read x",
		Environment:    "production",
		Stacktrace:     `{"values":[{"stacktrace":{"frames":[{"function":"main","filename":"a.go","lineno":10,"in_app":true}]}}]}`,
		Request:        `{"method":"POST","url":"https://app/x","headers":{"Accept":"*/*"}}`,
		Tags:           map[string]string{"server": "web1"},
	}
}

func TestRenderEventForLLM_Markdown(t *testing.T) {
	out := renderEventForLLM(issue.Issue{Title: "Boom"}, sampleEvent(), dumpMarkdown)
	for _, want := range []string{"# Boom", "## Exception", "TypeError: cannot read x",
		"## Stack trace", "a.go:10", "## Request", "POST https://app/x", "## Tags", "server=web1"} {
		if !strings.Contains(out, want) {
			t.Errorf("md dump missing %q\n---\n%s", want, out)
		}
	}
	if !strings.Contains(out, "2026-08-12T14:03:11Z") {
		t.Errorf("md dump missing RFC3339 time\n%s", out)
	}
	if !strings.Contains(out, "```") {
		t.Errorf("md dump has no code fences")
	}
}

func TestRenderEventForLLM_Plain(t *testing.T) {
	out := renderEventForLLM(issue.Issue{Title: "Boom"}, sampleEvent(), dumpPlain)
	if strings.Contains(out, "```") || strings.Contains(out, "## ") {
		t.Errorf("plain dump must have no markdown\n%s", out)
	}
	if !strings.Contains(out, "STACK TRACE") || !strings.Contains(out, "a.go:10") {
		t.Errorf("plain dump missing stack\n%s", out)
	}
}

func TestRenderEventForLLM_OmitsEmptySections(t *testing.T) {
	ev := event.Stored{ID: "x", Timestamp: time.Unix(0, 0).UTC(), Level: "info"}
	out := renderEventForLLM(issue.Issue{Title: "Bare"}, ev, dumpMarkdown)
	if strings.Contains(out, "## Stack trace") || strings.Contains(out, "## Request") {
		t.Errorf("empty sections must be omitted\n%s", out)
	}
}

func TestRenderEventForLLM_FenceGrowsWithBackticks(t *testing.T) {
	ev := sampleEvent()
	ev.ExceptionValue = "x ``` y ```` z" // 4 подряд
	out := renderEventForLLM(issue.Issue{Title: "T"}, ev, dumpMarkdown)
	if !strings.Contains(out, "`````") { // забор ≥5
		t.Errorf("fence did not grow past inner backticks\n%s", out)
	}
}

func TestRenderEventForLLM_CapTruncates(t *testing.T) {
	ev := sampleEvent()
	ev.ExceptionValue = strings.Repeat("A", maxDumpBytes+5000)
	out := renderEventForLLM(issue.Issue{Title: "T"}, ev, dumpPlain)
	if len(out) > maxDumpBytes+64 {
		t.Errorf("dump not capped: %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("cap marker missing")
	}
}

// Control-символы (кроме \n,\t) заменяются пробелом — иначе NUL и прочие
// невалидные в тексте символы попали бы в буфер/во вставку в LLM.
func TestRenderEventForLLM_SanitizesControlChars(t *testing.T) {
	ev := sampleEvent()
	ev.ExceptionValue = "bad\x00value\x07here"
	out := renderEventForLLM(issue.Issue{Title: "T"}, ev, dumpPlain)
	if strings.ContainsAny(out, "\x00\x07") {
		t.Errorf("control chars not sanitized:\n%q", out)
	}
	if !strings.Contains(out, "bad value here") {
		t.Errorf("expected control chars replaced by spaces:\n%s", out)
	}
	// \n и \t сохраняются.
	if !strings.Contains(out, "\n") {
		t.Errorf("newlines must be preserved")
	}
}

// Обрезка по капу режет строго по границе руны — многобайтовый UTF-8 не рвётся.
func TestRenderEventForLLM_CapKeepsRuneBoundary(t *testing.T) {
	ev := sampleEvent()
	// «я» — 2 байта; заполняем сверх капа, чтобы рез пришёлся внутрь руны.
	ev.ExceptionValue = strings.Repeat("я", maxDumpBytes)
	out := renderEventForLLM(issue.Issue{Title: "T"}, ev, dumpPlain)
	if !utf8.ValidString(out) {
		t.Errorf("cap broke a multibyte rune — output is not valid UTF-8")
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("cap marker missing")
	}
}
