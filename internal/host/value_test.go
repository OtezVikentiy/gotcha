package host_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// TestValueLabelPerKind закрепляет таблицу «вид порога → формат значения»
// (UX-аудит A1, P1-3): её читают и текст уведомления, и карточка хоста, и
// разъехаться им нечем, кроме забывчивости. Тишина проверяется на языке
// инстанса, потому что humanize.Duration локализована.
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
		// 3600 секунд — ровно тот случай, на котором карточка печатала
		// «3.6K» через fmtFloat: секунды без единицы измерения.
		{"silent", 3600, i18n.Tn(ru, "unit.hours", 1)},
		// Незнакомый вид (в Kinds не входит) — голое число, не паника.
		{"whatever", 1.5, "1.50"},
	}
	for _, c := range cases {
		if got := host.ValueLabel(ru, c.kind, c.v); got != c.want {
			t.Errorf("ValueLabel(%q, %v) = %q, want %q", c.kind, c.v, got, c.want)
		}
	}
}

// TestValueLabelCoversEveryKind — ни один вид из Kinds не должен печататься
// голым числом: это и был исходный дефект карточки (диск «0.93» при «93%» в
// списке строкой выше). Проверка идёт по Kinds, а не по списку литералов,
// чтобы новый вид порога не проехал мимо форматирования молча.
func TestValueLabelCoversEveryKind(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	for _, kind := range host.Kinds {
		got := host.ValueLabel(ru, kind, 0.5)
		if bare := host.ValueLabel(ru, "unknown-kind", 0.5); got == bare {
			t.Errorf("вид %q форматируется как незнакомый (%q) — нет юнита", kind, got)
		}
	}
}
