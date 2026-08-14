package web

import (
	"context"
	"math"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// chartGrid — общая сетка тестов этого файла: окно [from,to) с шагом step,
// выровненное к unix-эпохе (truncStepEpoch), чтобы ожидаемые индексы корзин
// считались руками, а не подгонялись под результат.
func chartGrid() (from, to time.Time, step time.Duration) {
	step = time.Minute
	from = time.Unix(1_700_000_000, 0).UTC().Truncate(step)
	to = from.Add(5 * step)
	return from, to, step
}

// seriesValueAt — значение ряда в корзине idx или NaN, если индекса нет.
func seriesValueAt(s NamedSeries, idx int) float64 {
	if idx < 0 || idx >= len(s.Points) {
		return math.NaN()
	}
	return s.Points[idx].V
}

// TestNamedSeriesFromGroupsAlignsGroupsToCommonGrid — ревью I4: группы с
// РАЗНЫМИ наборами непустых корзин обязаны выйти рядами одинаковой длины, с
// NaN там, где у группы данных нет.
//
// Без дозаполнения multiSeriesSVG растягивал каждый ряд на всю ширину по его
// собственной длине (xForIndex(j, len(s.Points))): точка монтирования,
// появившаяся в середине окна, рисовалась бы в масштабе всего графика, а общая
// полоса наведения показывала бы её значение под временем чужой серии.
func TestNamedSeriesFromGroupsAlignsGroupsToCommonGrid(t *testing.T) {
	from, to, step := chartGrid()

	// early — только первые две корзины, late — только последние две.
	groups := []metric.GroupedSeries{
		{Key: "/", Points: []metric.Point{
			{T: from, V: 0.10},
			{T: from.Add(step), V: 0.20},
		}},
		{Key: "/data", Points: []metric.Point{
			{T: from.Add(3 * step), V: 0.30},
			{T: from.Add(4 * step), V: 0.40},
		}},
	}

	series, legend := namedSeriesFromGroups(groups, 100, from, to, step)
	if len(series) != 2 || len(legend) != 2 {
		t.Fatalf("рядов/легенды %d/%d, want 2/2", len(series), len(legend))
	}
	if len(series[0].Points) != len(series[1].Points) {
		t.Fatalf("длины рядов разъехались: %d и %d — ряды нарисуются в разных временных масштабах",
			len(series[0].Points), len(series[1].Points))
	}
	// Окно [from, to] с шагом step включительно по обеим границам (fillSeries
	// идёт !t.After(to)) — шесть корзин.
	if got := len(series[0].Points); got != 6 {
		t.Fatalf("корзин в ряду %d, want 6 (окно 5 шагов, границы включительно)", got)
	}

	// Значения стоят в СВОИХ корзинах общей сетки и домножены на scale.
	for i, want := range []float64{10, 20, math.NaN(), math.NaN(), math.NaN(), math.NaN()} {
		got := seriesValueAt(series[0], i)
		if math.IsNaN(want) != math.IsNaN(got) || (!math.IsNaN(want) && got != want) {
			t.Errorf("ряд «/», корзина %d = %v, want %v", i, got, want)
		}
	}
	for i, want := range []float64{math.NaN(), math.NaN(), math.NaN(), 30, 40, math.NaN()} {
		got := seriesValueAt(series[1], i)
		if math.IsNaN(want) != math.IsNaN(got) || (!math.IsNaN(want) && got != want) {
			t.Errorf("ряд «/data», корзина %d = %v, want %v", i, got, want)
		}
	}

	// Время корзины — общее для одного индекса у обоих рядов: именно на этом
	// держится общая полоса наведения (одна подсказка на индекс сетки).
	for i := range series[0].Points {
		if !series[0].Points[i].T.Equal(series[1].Points[i].T) {
			t.Errorf("корзина %d: время рядов разное (%v и %v) — подсказка склеит чужие значения",
				i, series[0].Points[i].T, series[1].Points[i].T)
		}
	}
}

// TestNamedSeriesFromGroupsScaleOne — scale=1 (байт/с, штуки) не трогает
// значения, но сетку всё равно выравнивает: раньше при scale=1 точки шли в
// multiSeriesSVG вообще как есть.
func TestNamedSeriesFromGroupsScaleOne(t *testing.T) {
	from, to, step := chartGrid()
	groups := []metric.GroupedSeries{
		{Key: "receive", Points: []metric.Point{{T: from.Add(2 * step), V: 1024}}},
		{Key: "transmit", Points: []metric.Point{
			{T: from, V: 1}, {T: from.Add(step), V: 2}, {T: from.Add(2 * step), V: 3},
		}},
	}

	series, _ := namedSeriesFromGroups(groups, 1, from, to, step)
	if len(series[0].Points) != len(series[1].Points) {
		t.Fatalf("длины рядов %d и %d, want равные", len(series[0].Points), len(series[1].Points))
	}
	if got := seriesValueAt(series[0], 2); got != 1024 {
		t.Errorf("scale=1 изменил значение: корзина 2 = %v, want 1024", got)
	}
	if got := seriesValueAt(series[0], 0); !math.IsNaN(got) {
		t.Errorf("корзина 0 ряда receive = %v, want NaN (данных нет)", got)
	}
}

// TestHostLoadSeriesPartialFailure — отложенный minor T15 того же класса: одна
// из трёх серий load average пуста (коллектор не отдаёт 15m). Остальные две
// обязаны нарисоваться в правильном масштабе, а пустая — стать сплошным
// разрывом той же длины, а не сжать соседей и не пропасть из легенды.
func TestHostLoadSeriesPartialFailure(t *testing.T) {
	from, to, step := chartGrid()
	labels := []string{"1m", "5m", "15m"}
	raw := [][]metric.Point{
		{{T: from, V: 0.5}, {T: from.Add(step), V: 0.7}, {T: from.Add(2 * step), V: 0.9}},
		{{T: from, V: 0.4}, {T: from.Add(2 * step), V: 0.8}},
		nil, // 15m коллектор не прислал
	}

	series, legend := hostLoadSeries(raw, labels, from, to, step)
	if len(series) != 3 || len(legend) != 3 {
		t.Fatalf("рядов/легенды %d/%d, want 3/3 (пустой ряд не выпадает)", len(series), len(legend))
	}
	n := len(series[0].Points)
	for i, s := range series {
		if len(s.Points) != n {
			t.Fatalf("ряд %q длиной %d, остальные %d — масштабы разъехались", s.Label, len(s.Points), n)
		}
		if s.Label != labels[i] || legend[i].Label != labels[i] {
			t.Errorf("подпись ряда %d = %q/%q, want %q", i, s.Label, legend[i].Label, labels[i])
		}
	}

	// 5m: данные в корзинах 0 и 2, разрыв в корзине 1 — ровно там, где точки нет.
	if got := seriesValueAt(series[1], 1); !math.IsNaN(got) {
		t.Errorf("5m, корзина 1 = %v, want NaN (пропуск внутри ряда — разрыв, а не сдвиг соседних точек)", got)
	}
	if got := seriesValueAt(series[1], 2); got != 0.8 {
		t.Errorf("5m, корзина 2 = %v, want 0.8 (точка осталась на своём времени)", got)
	}
	// 15m: пусто целиком — все корзины NaN.
	for i := range series[2].Points {
		if !math.IsNaN(series[2].Points[i].V) {
			t.Fatalf("15m, корзина %d = %v, want NaN — ряда нет вовсе", i, series[2].Points[i].V)
		}
	}
}

// TestHostGroupLabelKeysResolve — все i18n-ключи легенды графиков хоста
// (hostGroupLabelKeys) обязаны существовать в ОБЕИХ локалях: карта не
// литеральный вызов i18n.T, поэтому общий сканер каталога
// (guards/i18n_keys_test.go) её не видит — тот же приём, каким закреплена
// availabilityBarLabelKey (svg_theme_test.go).
func TestHostGroupLabelKeysResolve(t *testing.T) {
	if len(hostGroupLabelKeys) == 0 {
		t.Fatal("карта подписей легенды пуста — проверять нечего")
	}
	for _, lang := range []string{"ru", "en"} {
		ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: lang})
		for value, key := range hostGroupLabelKeys {
			if got := i18n.T(ctx, key); got == key {
				t.Errorf("[%s] значение %q → ключ %q без перевода: в легенде будет сырой ключ", lang, value, key)
			}
		}
	}
}

// TestLocalizeGroupLabelsKeepsUnknownValues — незнакомое значение атрибута
// (набор status у hostmetricsreceiver зависит от версии и ядра) обязано
// доехать до легенды КАК ЕСТЬ, а не превратиться в сырой i18n-ключ. Ровно
// поэтому подписи берутся из карты, а не конкатенацией префикса со значением.
func TestLocalizeGroupLabelsKeepsUnknownValues(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	series := []NamedSeries{{Label: "read"}, {Label: "hypervisor-hiccup"}}
	legend := []templates.LegendItem{{Label: "read"}, {Label: "hypervisor-hiccup"}}

	localizeGroupLabels(ctx, series, legend)

	if series[0].Label == "read" || series[0].Label != legend[0].Label {
		t.Errorf("known value not localized in lockstep: series=%q legend=%q", series[0].Label, legend[0].Label)
	}
	if series[1].Label != "hypervisor-hiccup" || legend[1].Label != "hypervisor-hiccup" {
		t.Errorf("unknown value must stay raw: series=%q legend=%q", series[1].Label, legend[1].Label)
	}
}
