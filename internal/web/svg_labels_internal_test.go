package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestChartLabelsMatchWhatTheyDraw — подпись графика для скринридера обязана
// называть то, что нарисовано, и совпадать между пустой и заполненной версией.
//
// Три графика называли себя чужими именами: пропускная способность объявлялась
// то задержкой (пустая), то частотой событий (заполненная); гистограмма
// распределения — пропускной способностью и частотой, хотя это вообще не
// временной ряд; стек фаз задержки в пустом виде назывался графиком Web Vital.
// В отрисованной странице два соседних графика получали ОДНУ подпись — для
// вспомогательной технологии они были неразличимы. Ключ a11y.chart.histogram
// при этом не использовался нигде.
func TestChartLabelsMatchWhatTheyDraw(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	now := time.Now()

	cases := []struct {
		name     string
		empty    string
		full     string
		wantKey  string
		wrongKey []string
	}{
		{
			name:     "пропускная способность",
			empty:    throughputBarsMarkup(ctx, nil, 720, 200),
			full:     throughputBarsMarkup(ctx, []trace.LatencyPoint{{T: now, Count: 5}}, 720, 200),
			wantKey:  "a11y.chart.throughput",
			wrongKey: []string{"a11y.chart.latency", "a11y.chart.frequency"},
		},
		{
			name:     "гистограмма длительностей",
			empty:    durationHistogramMarkup(ctx, nil, 720, 200),
			full:     durationHistogramMarkup(ctx, []trace.DurationBucket{{UpperUS: 10000, Count: 3}}, 720, 200),
			wantKey:  "a11y.chart.histogram",
			wrongKey: []string{"a11y.chart.throughput", "a11y.chart.frequency"},
		},
	}

	for _, c := range cases {
		want := i18n.T(ctx, c.wantKey)
		for _, variant := range []struct {
			kind string
			svg  string
		}{{"пустой", c.empty}, {"заполненный", c.full}} {
			if !strings.Contains(variant.svg, `aria-label="`+want+`"`) {
				t.Errorf("%s (%s): подпись не %q", c.name, variant.kind, want)
			}
			for _, wrong := range c.wrongKey {
				if strings.Contains(variant.svg, `aria-label="`+i18n.T(ctx, wrong)+`"`) {
					t.Errorf("%s (%s): подписан чужим именем %q", c.name, variant.kind, wrong)
				}
			}
		}
	}
}

// TestEmptySparklineSitsOnBaseline — заглушка «нет данных» рисуется по базовой
// линии, а не по середине холста.
//
// Середина читалась как реальное среднее значение, причём ВЫШЕ настоящих нулей:
// у ряда с данными пустые корзины лежат на самом низу. Для продукта мониторинга
// это инверсия смысла — тишина выглядела активнее нуля.
func TestEmptySparklineSitsOnBaseline(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	const h = 24
	svg := flatlineSVG(ctx, 96, h)

	if strings.Contains(svg, `points="0,12 96,12"`) || strings.Contains(svg, `0,12.0`) {
		t.Fatalf("заглушка нарисована по середине холста:\n%s", svg)
	}
	if !strings.Contains(svg, "23.5") {
		t.Fatalf("заглушка не на базовой линии (ожидалось y=23.5 при h=24):\n%s", svg)
	}
	// И у неё своя подпись: пустой и заполненный спарклайн не должны быть
	// неразличимы для скринридера.
	if !strings.Contains(svg, i18n.T(ctx, "a11y.chart.sparkline_empty")) {
		t.Errorf("у пустого спарклайна подпись как у заполненного:\n%s", svg)
	}
}
