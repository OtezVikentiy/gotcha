package deploy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Обрезка по рунам, не байтам — иначе можно разорвать многобайтовый UTF-8
// символ (кириллица, эмодзи) посередине.
func TestCapStr(t *testing.T) {
	if got := capStr("abc", 10); got != "abc" {
		t.Errorf("capStr короткой строки = %q, want abc", got)
	}
	if got := capStr("abcde", 5); got != "abcde" {
		t.Errorf("capStr ровно лимит = %q, want abcde", got)
	}

	src := "абвгде"
	got := capStr(src, 3)
	if utf8.RuneCountInString(got) != 3 {
		t.Fatalf("capStr(%q, 3) = %q, want 3 руны", src, got)
	}
	if got != "абв" {
		t.Fatalf("capStr(%q, 3) = %q, want абв", src, got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("capStr порвал UTF-8: %q", got)
	}

	emoji := strings.Repeat("🚀", 4)
	cut := capStr(emoji, 2)
	if utf8.RuneCountInString(cut) != 2 || !utf8.ValidString(cut) {
		t.Fatalf("capStr эмодзи = %q (%d рун), want 2 валидные руны", cut, utf8.RuneCountInString(cut))
	}
}
