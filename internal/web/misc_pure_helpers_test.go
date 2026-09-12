package web

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

func TestEscalationsErrorMessage(t *testing.T) {
	ctx := ruTestCtx()
	if got, want := escalationsErrorMessage(ctx, escalation.ErrInvalidPolicy),
		i18n.T(ctx, "err.escalations.invalid"); got != want {
		t.Errorf("ErrInvalidPolicy = %q, want %q", got, want)
	}
	if got, want := escalationsErrorMessage(ctx, errors.New("unrelated")),
		i18n.T(ctx, "error.action_failed"); got != want {
		t.Errorf("unrelated error = %q, want %q", got, want)
	}
}

func TestSafeExternalHref(t *testing.T) {
	cases := []struct {
		in       string
		wantHref string
		wantOK   bool
	}{
		{"https://ci.example.com/run/1", "https://ci.example.com/run/1", true},
		{"http://ci.example.com/run/1", "http://ci.example.com/run/1", true},
		{"", "", false},
		{"javascript:alert(1)", "", false},
		{"file:///etc/passwd", "", false},
		{"data:text/html,<script>alert(1)</script>", "", false},
		{"://bad url\x7f", "", false},
	}
	for _, c := range cases {
		href, ok := safeExternalHref(c.in)
		if href != c.wantHref || ok != c.wantOK {
			t.Errorf("safeExternalHref(%q) = %q/%v, want %q/%v", c.in, href, ok, c.wantHref, c.wantOK)
		}
	}
}

func TestIsRuneBoundary(t *testing.T) {
	s := "a" + "€"
	if !isRuneBoundary(s, 0) {
		t.Error("i=0 must always be a boundary")
	}
	if !isRuneBoundary(s, len(s)) {
		t.Error("i=len(s) must always be a boundary")
	}
	if !isRuneBoundary(s, 1) {
		t.Error("i=1 (start of €) must be a boundary")
	}
	if isRuneBoundary(s, 2) {
		t.Error("i=2 (middle of €, continuation byte) must NOT be a boundary")
	}
	if isRuneBoundary(s, 3) {
		t.Error("i=3 (middle of €, continuation byte) must NOT be a boundary")
	}
}

func TestCapDump(t *testing.T) {
	short := "hello"
	if got := capDump(short); got != short {
		t.Errorf("capDump(short) = %q, want unchanged %q", got, short)
	}

	exact := strings.Repeat("a", maxDumpBytes)
	if got := capDump(exact); got != exact {
		t.Errorf("capDump(exactly maxDumpBytes) must be unchanged, got len=%d", len(got))
	}

	long := strings.Repeat("€", maxDumpBytes)
	got := capDump(long)
	if !strings.HasSuffix(got, capDumpMarker) {
		t.Fatalf("capDump(long) must end with the truncation marker, got tail: %q", got[max(0, len(got)-20):])
	}
	body := strings.TrimSuffix(got, capDumpMarker)
	if len(body) > maxDumpBytes {
		t.Errorf("capDump(long) body len = %d, want <= %d", len(body), maxDumpBytes)
	}
	if !utf8.ValidString(body) {
		t.Errorf("capDump(long) body must not end mid-rune (must stay valid UTF-8), tail: %q", body[len(body)-6:])
	}
}

func TestSanitizeControl(t *testing.T) {
	in := "line1\nline2\ttab\x01\x02end"
	want := "line1\nline2\ttab  end"
	if got := sanitizeControl(in); got != want {
		t.Errorf("sanitizeControl(%q) = %q, want %q", in, got, want)
	}
}

func TestPrettyJSON(t *testing.T) {
	if got := prettyJSON(""); got != "" {
		t.Errorf("prettyJSON(empty) = %q, want empty", got)
	}
	if got := prettyJSON("  "); got != "" {
		t.Errorf("prettyJSON(blank) = %q, want empty", got)
	}
	if got := prettyJSON("{}"); got != "" {
		t.Errorf("prettyJSON({}) = %q, want empty", got)
	}
	if got := prettyJSON("null"); got != "" {
		t.Errorf("prettyJSON(null) = %q, want empty", got)
	}
	if got := prettyJSON("{not json"); got != "" {
		t.Errorf("prettyJSON(invalid) = %q, want empty", got)
	}
	got := prettyJSON(`{"a":1,"b":"<x>&y"}`)
	if !strings.Contains(got, "\n") {
		t.Errorf("prettyJSON(valid) must be multi-line (indented), got %q", got)
	}
	if !strings.Contains(got, "<x>&y") {
		t.Errorf("prettyJSON must not HTML-escape values, got %q", got)
	}
}
