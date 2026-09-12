package web

import "testing"

// Верхняя граница maxPage — защита от `?page=<огромное>`, которое иначе даёт
// (page-1)*perPage с переполнением int и отрицательный SQL OFFSET.
func TestParsePageBounds(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 1},
		{"0", 1},
		{"-5", 1},
		{"abc", 1},
		{"1", 1},
		{"7", 7},
		{"1000000", maxPage},
		{"1000001", maxPage},
		{"9223372036854775807", maxPage},
	}
	for _, c := range cases {
		if got := parsePage(c.in); got != c.want {
			t.Errorf("parsePage(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	const perPage = 100
	if off := (parsePage("9223372036854775807") - 1) * perPage; off < 0 {
		t.Fatalf("offset = %d, отрицательный OFFSET снова возможен", off)
	}
}
