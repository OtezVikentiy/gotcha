package host_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

func TestValueLabelPerKind(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	cases := []struct {
		kind string
		v    float64
		want string
	}{
		{"disk", 0.93, "93.0%"},
		{"memory", 0.955, "95.5%"},
		{"load", 2.5, "2.50×"},
		{"silent", 3600, i18n.Tn(ru, "unit.hours", 1)},
		{"whatever", 1.5, "1.50"},
	}
	for _, c := range cases {
		if got := host.ValueLabel(ru, c.kind, c.v); got != c.want {
			t.Errorf("ValueLabel(%q, %v) = %q, want %q", c.kind, c.v, got, c.want)
		}
	}
}

func TestValueLabelCoversEveryKind(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	for _, kind := range host.Kinds {
		got := host.ValueLabel(ru, kind, 0.5)
		if bare := host.ValueLabel(ru, "unknown-kind", 0.5); got == bare {
			t.Errorf("вид %q форматируется как незнакомый (%q) — нет юнита", kind, got)
		}
	}
}
