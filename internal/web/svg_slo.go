package web

import (
	"context"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
)

// sloBurndownWidth/Height — размер SVG burn-down графика на экране деталей SLO.
// Ширина 1200 — как у графиков перцентилей/метрик (класс осей chart-vb1200).
const (
	sloBurndownWidth  = 1200
	sloBurndownHeight = 260
)

// sloBudgetPct форматирует долю бюджета как процент без дробной части:
// 1.0 → "100%", 0 → "0%", -4.0 → "-400%" (перерасход). Отдельная от fmtPct
// (templates) функция: там процент уже умножен на 100, здесь на входе доля.
func sloBudgetPct(frac float64) string {
	return strconv.FormatFloat(frac*100, 'f', 0, 64) + "%"
}

// sloBudgetBurndownSVG рисует burn-down график остатка error budget за окно SLO:
// накопительный остаток бюджета (в процентах) по времени. В каждой корзине
// остаток считается по ВСЕМ корзинам окна от начала до неё
// (slo.BudgetRemainingFraction над суммой good/total): линия стартует у 100%
// (бюджет цел) и убывает по мере накопления плохих событий. Ниже линии 0%
// (бюджет исчерпан) — красная зона перерасхода. Текст SVG состоит из чисел и
// html-экранированных подписей — templ.Raw безопасен, как у прочих графиков
// этого пакета.
func sloBudgetBurndownSVG(ctx context.Context, points []slo.Bucket, target float64, w, h int) templ.Component {
	return templ.Raw(sloBudgetBurndownMarkup(ctx, points, target, w, h))
}

func sloBudgetBurndownMarkup(ctx context.Context, points []slo.Bucket, target float64, w, h int) string {
	g := newChartGeom(w, h, 58, 16, 12, 26)

	var sb strings.Builder
	sb.WriteString(svgRoot("slo-burndown", w, h, i18n.T(ctx, "a11y.chart.slo_burndown")))

	// Накопительный остаток бюджета в каждой корзине: суммируем good/total от
	// начала окна и считаем долю остатка по этой сумме. Пустой префикс (Total==0,
	// событий ещё не было) — разрыв линии, а не мнимый ноль.
	type burnPoint struct {
		t   time.Time
		rem float64
		has bool
	}
	bpts := make([]burnPoint, len(points))
	var cumGood, cumTotal uint64
	haveData := false
	minRem := 1.0
	for i, b := range points {
		cumGood += b.Good
		cumTotal += b.Total
		rem, ok := slo.BudgetRemainingFraction([]slo.Bucket{{Good: cumGood, Total: cumTotal}}, target)
		if !ok {
			bpts[i] = burnPoint{t: b.T}
			continue
		}
		bpts[i] = burnPoint{t: b.T, rem: rem, has: true}
		haveData = true
		if rem < minRem {
			minRem = rem
		}
	}

	if !haveData {
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord((g.x0 + g.x1) / 2))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord((g.y0 + g.y1) / 2))
		sb.WriteString(`" text-anchor="middle" dominant-baseline="middle" fill="currentColor">`)
		sb.WriteString(html.EscapeString(i18n.T(ctx, "chart.no_data_period")))
		sb.WriteString(`</text></svg>`)
		return sb.String()
	}

	// Домен Y: верх фиксирован на 100% (полный бюджет — линия не может быть выше),
	// низ — 0% или ниже, если был перерасход. Небольшой запас сверху/снизу, чтобы
	// линия не липла к рамке.
	top, bottom := 1.0, 0.0
	if minRem < 0 {
		bottom = minRem
	}
	pad := (top - bottom) * 0.08
	top += pad
	bottom -= pad
	yFor := func(v float64) float64 {
		return g.y1 - (v-bottom)/(top-bottom)*(g.y1-g.y0)
	}

	// Красная зона перерасхода: всё ниже линии 0% (бюджет исчерпан). Рисуем
	// только когда остаток реально уходил в минус — иначе узкая полоса запаса под
	// нулём пугала бы зря. Зона идёт первой (под сеткой и линией данных).
	if minRem < 0 {
		zeroY := yFor(0)
		sb.WriteString(`<rect class="slo-burndown-overspend" x="`)
		sb.WriteString(formatCoord(g.x0))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(zeroY))
		sb.WriteString(`" width="`)
		sb.WriteString(formatCoord(g.x1 - g.x0))
		sb.WriteString(`" height="`)
		sb.WriteString(formatCoord(g.y1 - zeroY))
		sb.WriteString(`"/>`)
	}

	// Оси и сетка: рамка + горизонтали 100/50/0% с подписями в процентах.
	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	for _, lvl := range []float64{1.0, 0.5, 0.0} {
		if lvl > top || lvl < bottom {
			continue
		}
		y := yFor(lvl)
		axisLine(&sb, g.x0, y, g.x1, y)
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(g.x0 - 6))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(y))
		sb.WriteString(`" text-anchor="end" dominant-baseline="middle" fill="currentColor">`)
		sb.WriteString(html.EscapeString(sloBudgetPct(lvl)))
		sb.WriteString(`</text>`)
	}
	times := make([]time.Time, len(points))
	for i, b := range points {
		times[i] = b.T
	}
	n := len(times)
	writeXTicks(&sb, g, timeAxis(times, func(i int) float64 { return g.xForIndex(i, n) }, 70))
	sb.WriteString(`</g>`)

	// Линия остатка бюджета с мягкой заливкой под ней; разрывы на пустых
	// префиксах (has=false). Цвет линии — из CSS (.slo-burndown-line), заливка —
	// currentColor (.slo-burndown), как у остальных графиков.
	line := make([]seriesPoint, len(bpts))
	for i, bp := range bpts {
		x := g.xForIndex(i, len(bpts))
		if !bp.has {
			line[i] = seriesPoint{x: x, has: false}
			continue
		}
		line[i] = seriesPoint{x: x, y: yFor(bp.rem), has: true}
	}
	writeLineWithArea(&sb, line, g.y1, "currentColor", "gradSloBurndown", `class="slo-burndown-line"`)

	// Полосы наведения: остаток бюджета в каждой корзине. humanize.Time — без
	// собственного макета времени (format-guard), как в multiSeriesMarkup.
	band := (g.x1 - g.x0) / float64(n)
	for i, bp := range bpts {
		if !bp.has {
			continue
		}
		writeHoverBand(&sb, g, g.xForIndex(i, n)-band/2, band,
			humanize.Time(ctx, bp.t, time.UTC)+" — "+sloBudgetPct(bp.rem))
	}

	sb.WriteString(`</svg>`)
	return sb.String()
}
