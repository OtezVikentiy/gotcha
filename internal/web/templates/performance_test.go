package templates

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestFormatThroughputPicksUnit — единица подбирается по величине. С
// фиксированным «в минуту» колонка показывала «0.0/min» во всех строках: при
// десятке транзакций за сутки это 0.007/мин, и один знак после запятой всё
// схлопывал в ноль.
func TestFormatThroughputPicksUnit(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	cases := []struct {
		perMin float64
		want   string
	}{
		{12, "12.0/min"},
		{1, "1.0/min"},
		{0.5, "30.0/h"},   // 0.5/мин = 30/час
		{0.02, "1.2/h"},   // на границе: 1.2 в час
		{0.007, "10.1/day"}, // раньше здесь было «0.0/min»
		{0, "0.0/day"},
	}
	for _, c := range cases {
		if got := formatThroughput(ctx, c.perMin); got != c.want {
			t.Errorf("formatThroughput(%v) = %q, want %q", c.perMin, got, c.want)
		}
	}
}

// TestEnvList — строка списка агрегирует транзакции по всем окружениям сразу,
// поэтому колонка показывает их перечислением, а не одним значением.
func TestEnvList(t *testing.T) {
	cases := []struct {
		envs []string
		want string
	}{
		{[]string{"production", "staging"}, "production, staging"},
		{[]string{"production"}, "production"},
		{nil, "—"},
	}
	for _, c := range cases {
		if got := envList(c.envs); got != c.want {
			t.Errorf("envList(%v) = %q, want %q", c.envs, got, c.want)
		}
	}
}
