package web

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestEscalationsErrorMessage — ErrInvalidPolicy переводится в свой текст,
// неизвестная ошибка — в общий error.action_failed.
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

// TestSafeExternalHref — только http/https проходят как есть; пустая
// строка, невалидный URL и прочие схемы (javascript:, file:, data:) —
// ok=false (защита от javascript:-XSS в ссылке на внешний CI-прогон).
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

// TestIsRuneBoundary — границы 0 и len(s) всегда считаются границами руны
// (defensive: capDump зовёт с limit, который может дойти до края);
// байт-продолжение UTF-8 (10xxxxxx) — не граница, ведущий байт — граница.
func TestIsRuneBoundary(t *testing.T) {
	s := "a" + "€" // 'a' (1 байт) + евро (3 байта: e2 82 ac)
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

// TestCapDump — s короче/равен потолку возвращается как есть; длиннее —
// обрезается ДО границы руны с добавлением маркера, не разрывая
// многобайтовый символ пополам.
func TestCapDump(t *testing.T) {
	short := "hello"
	if got := capDump(short); got != short {
		t.Errorf("capDump(short) = %q, want unchanged %q", got, short)
	}

	exact := strings.Repeat("a", maxDumpBytes)
	if got := capDump(exact); got != exact {
		t.Errorf("capDump(exactly maxDumpBytes) must be unchanged, got len=%d", len(got))
	}

	// Строка длиннее потолка, где граница обрезки (maxDumpBytes-len(marker))
	// приходится РОВНО в середину многобайтового символа: '€' — 3 байта,
	// строка из одних '€' длиной больше потолка гарантированно ловит такой
	// случай на каком-то смещении около границы.
	long := strings.Repeat("€", maxDumpBytes) // намного длиннее потолка в байтах
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

// TestSanitizeControl — \n/\t проходят как есть, прочие control-руны
// заменяются пробелом, обычный текст не трогается.
func TestSanitizeControl(t *testing.T) {
	in := "line1\nline2\ttab\x01\x02end"
	want := "line1\nline2\ttab  end"
	if got := sanitizeControl(in); got != want {
		t.Errorf("sanitizeControl(%q) = %q, want %q", in, got, want)
	}
}

// TestPrettyJSON — пустая строка/{}/null — "" (нечего показывать); невалидный
// JSON — "" (не паника); валидный — с отступами, без экранирования HTML-
// символов (raw '&'/'<'/'>' — дамп для LLM, не для встраивания в HTML).
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
