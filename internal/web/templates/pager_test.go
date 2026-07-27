package templates

import "testing"

// TestPagerPrev (S1): ссылка «назад» на пустой (out-of-range) странице ведёт
// сразу на первую — иначе пользователь застревал бы на пустом экране без пути
// к данным. На обычной странице — на предыдущую.
func TestPagerPrev(t *testing.T) {
	cases := []struct {
		page  int
		total int64
		want  int
	}{
		{5, 0, 1},   // пустая out-of-range страница → на первую
		{999, 0, 1}, // руками введённый огромный page → на первую
		{5, 200, 4}, // обычная страница → на предыдущую
		{2, 30, 1},  // со второй → на первую
	}
	for _, c := range cases {
		if got := pagerPrev(c.page, c.total); got != c.want {
			t.Errorf("pagerPrev(%d, %d) = %d, want %d", c.page, c.total, got, c.want)
		}
	}
}
