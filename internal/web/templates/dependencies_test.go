package templates

import "testing"

// TestDepDirectionGlyph — глиф и ключ подсказки колонки «Данные» по всем
// значениям trace.DataDirection плюс неизвестное: прочерк и none — для пустой
// строки и для мусора, а не пустая ячейка.
func TestDepDirectionGlyph(t *testing.T) {
	cases := []struct {
		dir, glyph, key string
	}{
		{"in", "←", "deps.direction.in"},
		{"out", "→", "deps.direction.out"},
		{"both", "⇄", "deps.direction.both"},
		{"", "—", "deps.direction.none"},
		{"sideways", "—", "deps.direction.none"},
	}
	for _, c := range cases {
		if got := depDirectionGlyph(c.dir); got != c.glyph {
			t.Errorf("depDirectionGlyph(%q) = %q, ожидалось %q", c.dir, got, c.glyph)
		}
		if got := depDirectionKey(c.dir); got != c.key {
			t.Errorf("depDirectionKey(%q) = %q, ожидалось %q", c.dir, got, c.key)
		}
	}
}
