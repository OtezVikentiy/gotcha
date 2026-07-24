package web

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestMultiColourChartsEmitClassesNotHex — многоцветные графики должны
// краситься из app.css по классу. Пока цвета были впечатаны hex-литералами
// тёмной палитры, в светлой теме полоска доступности и waterfall оставались
// тёмными: одного currentColor там мало (нужно несколько цветов в одном SVG),
// и это «мало» раньше решали хардкодом.
func TestMultiColourChartsEmitClassesNotHex(t *testing.T) {
	ctx := context.Background()
	bars := []uptime.UptimeStat{
		{Total: 10, OK: 10}, // все успешны → зелёная
		{Total: 10, OK: 9},  // мелкие сбои, большинство ок → жёлтая
		{Total: 10, OK: 3},  // большинство провалилось → красная
		{Total: 0},          // проверок не было → серая
	}

	got := availabilityBarsMarkup(ctx, bars, 300, 24)

	for _, hex := range []string{"#2ea043", "#f0574a", "#263041"} {
		if strings.Contains(got, hex) {
			t.Errorf("в полоске доступности остался хардкод-цвет %s", hex)
		}
	}
	for _, cls := range []string{"bar-up", "bar-partial", "bar-down", "bar-empty"} {
		if !strings.Contains(got, cls) {
			t.Errorf("нет класса %q; got: %s", cls, got)
		}
	}

	// Пустая полоска — та же история: серый прямоугольник «нет данных».
	empty := availabilityBarsMarkup(ctx, nil, 300, 24)
	if strings.Contains(empty, "#263041") || !strings.Contains(empty, "bar-empty") {
		t.Errorf("пустая полоска не переведена на класс: %s", empty)
	}
}

// TestAvailabilityBarClassThresholds пришпиливает пороги окраски корзины:
// зелёная — все ок, жёлтая — большинство ок при наличии сбоев (граница ровно
// 50%), красная — большинство провалилось, серая — нет данных.
func TestAvailabilityBarClassThresholds(t *testing.T) {
	cases := []struct {
		stat uptime.UptimeStat
		want string
	}{
		{uptime.UptimeStat{Total: 0, OK: 0}, "bar-empty"},
		{uptime.UptimeStat{Total: 10, OK: 10}, "bar-up"},
		{uptime.UptimeStat{Total: 10, OK: 9}, "bar-partial"},
		{uptime.UptimeStat{Total: 10, OK: 5}, "bar-partial"}, // ровно 50% → жёлтая
		{uptime.UptimeStat{Total: 10, OK: 4}, "bar-down"},    // ниже 50% → красная
		{uptime.UptimeStat{Total: 10, OK: 0}, "bar-down"},
	}
	for _, c := range cases {
		if got := availabilityBarClass(c.stat); got != c.want {
			t.Errorf("availabilityBarClass(%+v) = %q, want %q", c.stat, got, c.want)
		}
	}
}

// TestChartColourClassesAreStyled — класс без правила в app.css красит SVG
// ничем: элемент просто отрисуется чёрным/прозрачным. Проверяем, что каждому
// классу назначен цвет.
func TestChartColourClassesAreStyled(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatal(err)
	}
	for _, cls := range []string{
		"bar-up", "bar-partial", "bar-down", "bar-empty",
		"wf-ok", "wf-err",
		"series-p50", "series-p95",
		"seg-dns", "seg-connect", "seg-tls", "seg-ttfb",
	} {
		if !strings.Contains(css, "."+cls) {
			t.Errorf("классу %q не назначен цвет в app.css", cls)
		}
	}
}
