package templates

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

func TestFormatThroughputPicksUnit(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "en"})
	cases := []struct {
		perMin float64
		want   string
	}{
		{12, "12.0/min"},
		{1, "1.0/min"},
		{0.5, "30.0/h"},
		{0.02, "1.2/h"},
		{0.007, "10.1/day"},
		{0, "0.0/day"},
	}
	for _, c := range cases {
		if got := formatThroughput(ctx, c.perMin); got != c.want {
			t.Errorf("formatThroughput(%v) = %q, want %q", c.perMin, got, c.want)
		}
	}
}

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
