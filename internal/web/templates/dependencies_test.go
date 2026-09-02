package templates

import "testing"

// TestDepDirectionIcon — иконка и ключ подсказки колонки «Данные» по всем
// значениям trace.DataDirection плюс неизвестное: minus и none — для пустой
// строки и для мусора, а не пустая ячейка.
func TestDepDirectionIcon(t *testing.T) {
	cases := []struct {
		dir, icon, key string
	}{
		{"in", "arrow-left", "deps.direction.in"},
		{"out", "arrow-right", "deps.direction.out"},
		{"both", "arrow-left-right", "deps.direction.both"},
		{"", "minus", "deps.direction.none"},
		{"sideways", "minus", "deps.direction.none"},
	}
	for _, c := range cases {
		if got := depDirectionIcon(c.dir); got != c.icon {
			t.Errorf("depDirectionIcon(%q) = %q, ожидалось %q", c.dir, got, c.icon)
		}
		if got := depDirectionKey(c.dir); got != c.key {
			t.Errorf("depDirectionKey(%q) = %q, ожидалось %q", c.dir, got, c.key)
		}
	}
}
