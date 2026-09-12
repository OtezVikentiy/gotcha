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

// ширина 1200 — как у графиков перцентилей/метрик (класс осей chart-vb1200).
const (
	sloBurndownWidth  = 1200
	sloBurndownHeight = 260
)

// доля, не проценты — в отличие от fmtPct (templates), где вход уже умножен на 100.
func sloBudgetPct(frac float64) string {
	return strconv.FormatFloat(frac*100, 'f', 0, 64) + "%"
}

// templ.Raw безопасен: SVG строится из чисел и html-экранированных подписей.
func sloBudgetBurndownSVG(ctx context.Context, points []slo.Bucket, target float64, w, h int) templ.Component {
	return templ.Raw(sloBudgetBurndownMarkup(ctx, points, target, w, h))
}

func sloBudgetBurndownMarkup(ctx context.Context, points []slo.Bucket, target float64, w, h int) string {
	g := newChartGeom(w, h, 58, 16, 12, 26)

	var sb strings.Builder
	sb.WriteString(svgRoot("slo-burndown", w, h, i18n.T(ctx, "a11y.chart.slo_burndown")))

	// Total==0 в префиксе — разрыв линии, не мнимый ноль.
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

	// верх фиксирован на 100% — линия остатка не бывает выше; низ 0% или
	// ниже при перерасходе, плюс небольшой запас, чтобы не липнуть к рамке.
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

	// считаем до первой отрисовки — «100%» шире поля 58, иначе резалось бы левым краем.
	var levels []float64
	var yLabels []string
	for _, lvl := range []float64{1.0, 0.5, 0.0} {
		if lvl > top || lvl < bottom {
			continue
		}
		levels = append(levels, lvl)
		yLabels = append(yLabels, sloBudgetPct(lvl))
	}
	g.x0 = yAxisPadL(g.w, g.x0, yLabels)

	// рисуем, только когда остаток реально уходил в минус — иначе узкая
	// полоса запаса под нулём пугала бы зря.
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

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	for _, lvl := range levels {
		y := yFor(lvl)
		axisLine(&sb, g.x0, y, g.x1, y)
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(g.x0 - yLabelGap))
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

	// разрывы линии на пустых префиксах (has=false).
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

	// humanize.Time без своего формата времени — как в multiSeriesMarkup.
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
