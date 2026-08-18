package deploy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCapStr — обрезка полей деплоя на приёме: по РУНАМ, не по байтам, чтобы не
// разорвать многобайтовый UTF-8 посередине символа (CI вполне способен прислать
// кириллический changelog). Короткая строка возвращается как есть.
func TestCapStr(t *testing.T) {
	// Короче лимита — не трогаем.
	if got := capStr("abc", 10); got != "abc" {
		t.Errorf("capStr короткой строки = %q, want abc", got)
	}
	// Ровно лимит — не трогаем.
	if got := capStr("abcde", 5); got != "abcde" {
		t.Errorf("capStr ровно лимит = %q, want abcde", got)
	}

	// Многобайтовые руны: 6 кириллических символов (2 байта каждый) режем до 3.
	src := "абвгде"
	got := capStr(src, 3)
	if utf8.RuneCountInString(got) != 3 {
		t.Fatalf("capStr(%q, 3) = %q, want 3 руны", src, got)
	}
	if got != "абв" {
		t.Fatalf("capStr(%q, 3) = %q, want абв", src, got)
	}
	// Результат — валидный UTF-8 (символ не разорван на половину байта).
	if !utf8.ValidString(got) {
		t.Fatalf("capStr порвал UTF-8: %q", got)
	}

	// Эмодзи (4 байта) — режем по рунам, не по байтам.
	emoji := strings.Repeat("🚀", 4)
	cut := capStr(emoji, 2)
	if utf8.RuneCountInString(cut) != 2 || !utf8.ValidString(cut) {
		t.Fatalf("capStr эмодзи = %q (%d рун), want 2 валидные руны", cut, utf8.RuneCountInString(cut))
	}
}
