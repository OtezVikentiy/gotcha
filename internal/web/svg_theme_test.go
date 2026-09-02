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

// TestAvailabilityBarPartialPlainFill: «частично» — одна заливка классом
// bar-partial, без оверлея и <pattern>: штриховка снята вместе с переходом на
// палитру по оттенку (см. CHANGELOG). На корзину — ровно один <rect>.
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

// TestAvailabilityBarClassThresholds пришпиливает пороги окраски корзины:
// зелёная — все ок, жёлтая — большинство ок при наличии сбоев (граница ровно
// 50%), красная — большинство провалилось, серая — нет данных.
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
	// Полнота таблицы: новый класс без подписи должен ронять тест, а не
	// молча получать пустую строку (как это делала ветвистая версия №29).
	// Резолв в ОБОИХ каталогах — тоже здесь: ключи таблицы не литеральные
	// вызовы i18n.T, общий сканер каталога (i18n_keys_test.go) их не видит —
	// тот же приём, каким TestDynamicKeysResolve страхует конкатенации.
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

// TestAvailabilityBarsStretchToCardWidth — полоска доступности рисуется в
// фиксированные 192×24 (availabilityBarsWidth/Height), а на публичной
// статус-странице карточка шире (646px/255px). Дефолтный
// preserveAspectRatio="xMidYMid meet" держит натуральный масштаб и
// центрирует SVG внутри карточки — полоска выглядит крошечной. object-fit
// тут не помогает (не замещаемый элемент, инлайновый корневой <svg>),
// поэтому растягивать обязан сам preserveAspectRatio="none" на ЭТОМ
// графике. Второй ассерт — обязательная защита от расползания: у ПРОИЗВОЛЬНОГО
// другого графика (chartBars) атрибут должен остаться отсутствующим
// (дефолтное поведение) — иначе правка молча тронула бы svgRoot() и все
// графики продукта разом.
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
