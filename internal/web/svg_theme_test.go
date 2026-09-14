package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestMultiColourChartsEmitClassesNotHex(t *testing.T) {
	ctx := context.Background()
	bars := []uptime.UptimeStat{
		{Total: 10, OK: 10},
		{Total: 10, OK: 9},
		{Total: 10, OK: 3},
		{Total: 0},
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

	empty := availabilityBarsMarkup(ctx, nil, 300, 24)
	if strings.Contains(empty, "#263041") || !strings.Contains(empty, "bar-empty") {
		t.Errorf("пустая полоска не переведена на класс: %s", empty)
	}
}

func TestAvailabilityBarPartialPlainFill(t *testing.T) {
	ctx := context.Background()
	bars := []uptime.UptimeStat{{Total: 10, OK: 9}, {Total: 10, OK: 10}, {Total: 10, OK: 2}}
	m := availabilityBarsMarkup(ctx, bars, 300, 28)
	for _, bad := range []string{"<pattern", "<defs", "url(#", "bar-hatch", "pointer-events"} {
		if strings.Contains(m, bad) {
			t.Errorf("в полоске остался след штриховки %q: %s", bad, m)
		}
	}
	if got := strings.Count(m, "<rect "); got != len(bars) {
		t.Errorf("на %d корзин %d <rect>, ожидается по одному: %s", len(bars), got, m)
	}
}

func TestAvailabilityBarClassThresholds(t *testing.T) {
	cases := []struct {
		stat    uptime.UptimeStat
		want    string
		wantKey string
	}{
		{uptime.UptimeStat{Total: 0, OK: 0}, "bar-empty", "chart.no_data"},
		{uptime.UptimeStat{Total: 10, OK: 10}, "bar-up", "chart.bar.up"},
		{uptime.UptimeStat{Total: 10, OK: 9}, "bar-partial", "chart.bar.partial"},
		{uptime.UptimeStat{Total: 10, OK: 5}, "bar-partial", "chart.bar.partial"}, // ровно 50% → жёлтая
		{uptime.UptimeStat{Total: 10, OK: 4}, "bar-down", "chart.bar.down"},       // ниже 50% → красная
		{uptime.UptimeStat{Total: 10, OK: 0}, "bar-down", "chart.bar.down"},
	}
	for _, c := range cases {
		if got := availabilityBarClass(c.stat); got != c.want {
			t.Errorf("availabilityBarClass(%+v) = %q, want %q", c.stat, got, c.want)
		}
		if got := availabilityBarLabelKey[availabilityBarClass(c.stat)]; got != c.wantKey {
			t.Errorf("labelKey(%+v) = %q, want %q", c.stat, got, c.wantKey)
		}
	}
	// ключи этой таблицы не литеральные вызовы i18n.T — статический сканер
	// каталога их не видит, резолв проверяется здесь.
	for _, cls := range []string{availabilityClassUp, availabilityClassPartial, availabilityClassDown, availabilityClassEmpty} {
		key := availabilityBarLabelKey[cls]
		if key == "" {
			t.Errorf("класс %s без i18n-ключа подписи", cls)
			continue
		}
		for _, lang := range []string{"ru", "en"} {
			ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
			if got := i18n.T(ctx, key); got == key {
				t.Errorf("[%s] ключ %q из availabilityBarLabelKey не резолвится — на полоске будет сырой ключ", lang, key)
			}
		}
	}
}

func TestAvailabilityBarLabelDistinguishesNeighbors(t *testing.T) {
	ctx := context.Background()
	a := availabilityBarLabel(ctx, uptime.UptimeStat{Total: 48, OK: 47})
	b := availabilityBarLabel(ctx, uptime.UptimeStat{Total: 48, OK: 30})
	if a == b {
		t.Fatalf("два столбика одного класса (partial), но разного OK/Total получили одинаковую подпись %q — на полосе из 90 они неотличимы", a)
	}
	if !strings.Contains(a, "47") || !strings.Contains(a, "48") {
		t.Errorf("availabilityBarLabel(48,47) = %q, ожидались числа OK и Total в тексте", a)
	}
	if empty := availabilityBarLabel(ctx, uptime.UptimeStat{}); strings.ContainsAny(empty, "0123456789") {
		t.Errorf("пустая корзина (Total=0) не должна печатать 0/0: %q", empty)
	}
}

// object-fit не работает на инлайновом корневом <svg> (не замещаемый элемент) —
// растягивать обязан сам preserveAspectRatio="none" на этом графике.
func TestAvailabilityBarsStretchToCardWidth(t *testing.T) {
	ctx := context.Background()

	populated := availabilityBarsMarkup(ctx, []uptime.UptimeStat{{Total: 10, OK: 10}}, 300, 24)
	if !strings.Contains(populated, `preserveAspectRatio="none"`) {
		t.Errorf("заполненная полоска доступности без preserveAspectRatio=\"none\": %s", populated)
	}
	empty := availabilityBarsMarkup(ctx, nil, 300, 24)
	if !strings.Contains(empty, `preserveAspectRatio="none"`) {
		t.Errorf("пустая полоска доступности без preserveAspectRatio=\"none\": %s", empty)
	}

	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	other := chartBars(ctx, []event.Point{{T: base, N: 3}, {T: base.Add(24 * time.Hour), N: 5}}, chartWidth, chartHeight)
	if strings.Contains(other, `preserveAspectRatio`) {
		t.Errorf("правка расползлась на другой график (chartBars): preserveAspectRatio не должен появляться там: %s", other)
	}
}

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
