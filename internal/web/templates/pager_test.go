package templates

import "testing"

func TestPagerPrev(t *testing.T) {
	cases := []struct {
		page  int
		total int64
		want  int
	}{
		{5, 0, 1},
		{999, 0, 1},
		{5, 200, 4},
		{2, 30, 1},
	}
	for _, c := range cases {
		if got := pagerPrev(c.page, c.total); got != c.want {
			t.Errorf("pagerPrev(%d, %d) = %d, want %d", c.page, c.total, got, c.want)
		}
	}
}
