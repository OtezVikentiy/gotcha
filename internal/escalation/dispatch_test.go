package escalation_test

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

// TestContainsID — чистая функция членства без БД: результат зависит от
// присутствия id в списке, порядка/длины списка и пустого списка отдельно.
func TestContainsID(t *testing.T) {
	cases := []struct {
		name string
		ids  []int64
		id   int64
		want bool
	}{
		{"present", []int64{1, 2, 3}, 2, true},
		{"absent", []int64{1, 2, 3}, 5, false},
		{"empty", nil, 1, false},
		{"single match", []int64{7}, 7, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := escalation.ContainsID(c.ids, c.id); got != c.want {
				t.Errorf("ContainsID(%v, %d) = %v, want %v", c.ids, c.id, got, c.want)
			}
		})
	}
}
