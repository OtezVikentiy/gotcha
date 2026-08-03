package web

import (
	"context"
	"regexp"
	"strings"
	"testing"

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

// TestAvailabilityBarPartialHatched: у «частично» два канала — цвет и
// штриховка (№28). Id паттерна уникален на документ — та же дисциплина, что у
// градиентов графиков (см. gradSeq): дубли id в двух SVG на странице красят
// оба паттерном первого.
func TestAvailabilityBarPartialHatched(t *testing.T) {
	ctx := context.Background()
	bars := []uptime.UptimeStat{{Total: 10, OK: 9}}
	m1 := availabilityBarsMarkup(ctx, bars, 300, 28)
	if !strings.Contains(m1, `<pattern id="barhatch-`) || !strings.Contains(m1, `fill="url(#barhatch-`) {
		t.Errorf("partial-корзина без штриховки: %s", m1)
	}
	if !strings.Contains(m1, `class="bar-hatch"`) {
		t.Errorf("в паттерне нет штриха класса bar-hatch: %s", m1)
	}
	m2 := availabilityBarsMarkup(ctx, bars, 300, 28)
	id := regexp.MustCompile(`barhatch-[a-z0-9]+`).FindString(m1)
	if id == "" || strings.Contains(m2, id) {
		t.Errorf("id паттерна не уникален между документами: %q и в m2", id)
	}
	noPartial := availabilityBarsMarkup(ctx, []uptime.UptimeStat{{Total: 10, OK: 10}}, 300, 28)
	if strings.Contains(noPartial, "<pattern") {
		t.Errorf("паттерн рисуется без partial-корзин: %s", noPartial)
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
