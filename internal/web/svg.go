package web

import (
	"context"
	"fmt"
	"hash/fnv"
	"html"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/humanize"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

const flameRowHeight = 18

// не svgCharWidthPx — та для подписей осей с кеглем по ширине окна; кегль
// флеймграфа фиксирован (svg.flamegraph text, 11px), измерено 6.6 на символ.
const flameCharWidthPx = 6.6

const flameLabelPad = 2

// role="img" + aria-label обязательны — без них скринридер объявляет график
// как безымянную графику, а <title> в <rect> без фокуса обычно не озвучивается.
func svgRoot(class string, w, h int, label string) string {
	var sb strings.Builder
	sb.WriteString(`<svg class="`)
	sb.WriteString(class)
	// кегль подписей — в единицах viewBox: один font-size на разные viewBox
	// давал разный видимый размер; класс проставляет сам генератор.
	sb.WriteString(` chart-vb`)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteString(`" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" role="img" aria-label="`)
	sb.WriteString(html.EscapeString(label))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)
	return sb.String()
}

// фокус (зум по клику): предки — на всю ширину, полупрозрачные; доля в
// тултипе всегда от корня, чтобы числа не «прыгали» при зуме.
func flamegraphSVG(ctx context.Context, root *profile.FlameNode, focusPath []string, width int, link func(path []string) string) templ.Component {
	if !flameHasData(root) {
		return templ.Raw(`<p class="empty">` + html.EscapeString(i18n.T(ctx, "profile.flame.no_data")) + `</p>`)
	}
	node, ancestors, ok := focusFlame(root, focusPath)
	if !ok {
		focusPath = nil
	}
	height := (len(ancestors) + flameDepth(node)) * flameRowHeight
	var sb strings.Builder
	sb.WriteString(svgRoot("flamegraph", width, height, i18n.T(ctx, "a11y.chart.flamegraph")))
	fw := float64(width)
	for i, a := range ancestors {
		// путь корня — nil, а не пустой срез: link(nil) обязан дать URL без focus.
		var path []string
		if i > 0 {
			path = focusPath[:i]
		}
		flameNode(&sb, a, 0, fw, i, root.Value, path, link, true)
	}
	flameRow(&sb, node, 0, fw, len(ancestors), root.Value, focusPath, link)
	sb.WriteString(`</svg>`)
	return templ.Raw(sb.String())
}

func flameHasData(root *profile.FlameNode) bool {
	return root != nil && root.Value > 0
}

// дети слиты по имени при сборке дерева — путь по именам однозначен.
func focusFlame(root *profile.FlameNode, path []string) (node *profile.FlameNode, ancestors []*profile.FlameNode, ok bool) {
	node = root
	for _, name := range path {
		var next *profile.FlameNode
		for _, c := range node.Children {
			if c.Name == name {
				next = c
				break
			}
		}
		if next == nil {
			return root, nil, false
		}
		ancestors = append(ancestors, node)
		node = next
	}
	return node, ancestors, true
}

func flameDepth(n *profile.FlameNode) int {
	max := 0
	for _, c := range n.Children {
		if d := flameDepth(c); d > max {
			max = d
		}
	}
	return max + 1
}

func flameRow(sb *strings.Builder, n *profile.FlameNode, x, w float64, depth int, total uint64, path []string, link func(path []string) string) {
	// n.Value==0 — узел без сэмплов; деление ниже дало бы детям NaN/Inf, не
	// отсекаемый guard'ом (NaN<0.5 — false).
	if w < 0.5 || n.Value == 0 {
		return
	}
	flameNode(sb, n, x, w, depth, total, path, link, false)
	childX := x
	for _, c := range n.Children {
		cw := w * float64(c.Value) / float64(n.Value)
		// свой срез каждому ребёнку — append к общему path делил бы буфер между братьями.
		cp := make([]string, len(path)+1)
		copy(cp, path)
		cp[len(path)] = c.Name
		flameRow(sb, c, childX, cw, depth+1, total, cp, link)
		childX += cw
	}
}

// вложенный <svg> клипует сам (overflow hidden) — подпись не вылезет за
// кадр даже при расхождении расчётной и реальной ширины символа, без clipPath/id.
func flameNode(sb *strings.Builder, n *profile.FlameNode, x, w float64, depth int, total uint64, path []string, link func(path []string) string, ancestor bool) {
	pct := 0.0
	if total > 0 {
		pct = float64(n.Value) / float64(total) * 100
	}
	sb.WriteString(`<a href="`)
	sb.WriteString(html.EscapeString(link(path)))
	sb.WriteString(`"><svg`)
	if ancestor {
		sb.WriteString(` class="flame-ancestor"`)
	}
	sb.WriteString(` x="`)
	sb.WriteString(formatCoord(x))
	sb.WriteString(`" y="`)
	sb.WriteString(strconv.Itoa(depth * flameRowHeight))
	sb.WriteString(`" width="`)
	sb.WriteString(formatCoord(w))
	sb.WriteString(`" height="`)
	sb.WriteString(strconv.Itoa(flameRowHeight - 1))
	sb.WriteString(`"><rect x="0" y="0" width="100%" height="100%" fill="`)
	sb.WriteString(flameColor(n.Name))
	sb.WriteString(`"><title>`)
	sb.WriteString(html.EscapeString(n.Name))
	sb.WriteString(` — `)
	sb.WriteString(strconv.FormatFloat(pct, 'f', 1, 64))
	sb.WriteString(`%</title></rect>`)
	if label := fitFlameLabel(n.Name, w); label != "" {
		sb.WriteString(`<text x="`)
		sb.WriteString(strconv.Itoa(flameLabelPad))
		sb.WriteString(`" y="`)
		sb.WriteString(strconv.Itoa(flameRowHeight - 6))
		sb.WriteString(`" fill="#111">`)
		sb.WriteString(html.EscapeString(label))
		sb.WriteString(`</text>`)
	}
	sb.WriteString(`</svg></a>`)
}

// усекает до fit-1 рун с «…»; если остаётся меньше трёх рун — подписи нет
// вовсе, читателю тултип.
func fitFlameLabel(name string, w float64) string {
	fit := int((w - 2*flameLabelPad) / flameCharWidthPx)
	r := []rune(name)
	if len(r) <= fit {
		return name
	}
	if fit-1 < 3 {
		return ""
	}
	return string(r[:fit-1]) + "…"
}

// hue начинается с 24, не с 0 — чистый красный (<20) занят под --danger,
// кадры флеймграфа не должны с ним путаться.
func flameColor(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	hue := int(h.Sum32()%26) + 24 // 24..49 — янтарь→оранжевый→золото, без красного
	return fmt.Sprintf("hsl(%d,70%%,58%%)", hue)
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type metricThreshold struct {
	Value      float64
	Comparator string // "gt" | "lt"
}

// templ.Raw безопасен: SVG строится из чисел и html-экранированных подписей.
func metricSeriesSVG(ctx context.Context, points []metric.Point, unit string, thresholds []metricThreshold, deploys []deploy.Deployment, w, h int) templ.Component {
	return templ.Raw(metricSeriesMarkup(ctx, points, unit, thresholds, deploys, w, h))
}

func metricSeriesMarkup(ctx context.Context, points []metric.Point, unit string, thresholds []metricThreshold, deploys []deploy.Deployment, w, h int) string {
	const (
		padL = 58 // место под подписи оси Y
		padR = 16
		padT = 12
		padB = 26 // место под подписи оси X
	)
	x0, x1 := float64(padL), float64(w-padR)
	y0, y1 := float64(padT), float64(h-padB)

	var sb strings.Builder
	sb.WriteString(svgRoot("metric-chart", w, h, i18n.T(ctx, "a11y.chart.metric")))

	// пустые NaN-корзины дозаполнения окна не входят в домен и рисуются
	// разрывом, не нулём.
	haveData := false
	var dataMin, dataMax float64
	for _, p := range points {
		// ±Inf рядом с NaN — иначе одна такая точка сделает domMax-domMin
		// бесконечным, и NaN-координаты получат все точки, не только сбойную.
		if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
			continue
		}
		if !haveData {
			dataMin, dataMax, haveData = p.V, p.V, true
			continue
		}
		if p.V < dataMin {
			dataMin = p.V
		}
		if p.V > dataMax {
			dataMax = p.V
		}
	}
	if !haveData {
		// группа осей открывается здесь отдельно от общего кода ниже (после
		// yAxisPadL) — иначе ранний выход оставлял бы осиротевший </g>.
		sb.WriteString(`<g class="chart-axis">`)
		axisLine(&sb, x0, y0, x0, y1)
		axisLine(&sb, x0, y1, x1, y1)
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord((x0 + x1) / 2))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord((y0 + y1) / 2))
		sb.WriteString(`" text-anchor="middle" dominant-baseline="middle" fill="currentColor">`)
		sb.WriteString(html.EscapeString(i18n.T(ctx, "chart.no_data_period")))
		sb.WriteString(`</text></g></svg>`)
		return sb.String()
	}
	domMin, domMax := dataMin, dataMax
	for _, t := range thresholds {
		if t.Value < domMin {
			domMin = t.Value
		}
		if t.Value > domMax {
			domMax = t.Value
		}
	}
	if domMax == domMin {
		domMin -= 1
		domMax += 1
	}
	pad := (domMax - domMin) * 0.08
	domMin -= pad
	domMax += pad
	yFor := func(v float64) float64 {
		return y1 - (v-domMin)/(domMax-domMin)*(y1-y0)
	}

	// плоский ряд (dataMin==dataMax) даёт три равных значения — рисуем одну
	// подпись/линию вместо трёх наложенных.
	yValues := []float64{dataMax, (dataMin + dataMax) / 2, dataMin}
	if dataMin == dataMax {
		yValues = []float64{dataMax}
	}
	// unit из OTLP может быть шире padL — без yAxisPadL подпись резалась бы левым краем.
	yLabels := make([]string, len(yValues))
	for i, v := range yValues {
		yLabels[i] = formatAxisValue(v, unit)
	}
	x0 = yAxisPadL(w, x0, yLabels)

	sb.WriteString(`<g class="chart-axis">`)
	axisLine(&sb, x0, y0, x0, y1)
	axisLine(&sb, x0, y1, x1, y1)

	for i, v := range yValues {
		yv := yFor(v)
		axisLine(&sb, x0, yv, x1, yv)
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(x0 - yLabelGap))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(yv))
		sb.WriteString(`" text-anchor="end" dominant-baseline="middle" fill="currentColor">`)
		sb.WriteString(html.EscapeString(yLabels[i]))
		sb.WriteString(`</text>`)
	}

	n := len(points)
	spanH := points[n-1].T.Sub(points[0].T).Hours()
	xLabel := func(t time.Time, xpos float64, anchor string) {
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(xpos))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(float64(h) - 8))
		sb.WriteString(`" text-anchor="`)
		sb.WriteString(anchor)
		sb.WriteString(`" fill="currentColor">`)
		sb.WriteString(html.EscapeString(metricTimeLabel(t, spanH)))
		sb.WriteString(`</text>`)
	}
	xLabel(points[0].T, x0, "start")
	if n > 2 {
		xLabel(points[n/2].T, (x0+x1)/2, "middle")
	}
	xLabel(points[n-1].T, x1, "end")
	sb.WriteString(`</g>`) // конец chart-axis

	for _, t := range thresholds {
		yv := yFor(t.Value)
		if yv < y0 || yv > y1 {
			continue
		}
		sb.WriteString(`<g class="chart-threshold"><line x1="`)
		sb.WriteString(formatCoord(x0))
		sb.WriteString(`" y1="`)
		sb.WriteString(formatCoord(yv))
		sb.WriteString(`" x2="`)
		sb.WriteString(formatCoord(x1))
		sb.WriteString(`" y2="`)
		sb.WriteString(formatCoord(yv))
		sb.WriteString(`" stroke="currentColor" stroke-width="1" stroke-dasharray="4 3"/><text x="`)
		sb.WriteString(formatCoord(x1 - 4))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(yv - 4))
		sb.WriteString(`" text-anchor="end" fill="currentColor">`)
		sb.WriteString(html.EscapeString(comparatorSymbol(t.Comparator) + " " + formatAxisValue(t.Value, unit)))
		sb.WriteString(`</text></g>`)
	}

	// NaN и ±Inf — разрыв линии (has=false), не провал в ноль: график
	// покрывает окно, но не рисует данных, где их нет.
	linePts := make([]seriesPoint, n)
	for i, p := range points {
		x := x0
		if n > 1 {
			x = x0 + float64(i)/float64(n-1)*(x1-x0)
		}
		if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
			linePts[i] = seriesPoint{x: x, has: false}
			continue
		}
		linePts[i] = seriesPoint{x: x, y: yFor(p.V), has: true}
	}
	// цвет — из CSS (color у .metric-chart), не хекс: зашитая копия
	// разошлась бы с токенами молча.
	writeLineWithArea(&sb, linePts, y1, "currentColor", "gradMetric", `stroke="currentColor"`)

	// линия тонкая, наводиться нечем — подсказку ловит прозрачная полоса над
	// интервалом; пустые корзины и ±Inf пропускаем.
	g := chartGeom{w: w, h: h, x0: x0, x1: x1, y0: y0, y1: y1}
	band := (x1 - x0) / float64(n)
	for i, p := range points {
		if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
			continue
		}
		x := x0
		if n > 1 {
			x = x0 + float64(i)/float64(n-1)*(x1-x0)
		}
		writeHoverBand(&sb, g, x-band/2, band,
			humanize.Time(ctx, p.T, time.UTC)+" — "+formatAxisValue(p.V, unit))
	}

	times := make([]time.Time, n)
	for i, p := range points {
		times[i] = p.T
	}
	writeDeployMarker(&sb, g, times, deploys)

	sb.WriteString(`</svg>`)
	return sb.String()
}

// i-й элемент рисуется классом series-m{i+1} — порядок в срезе задаёт и легенду, и палитру.
type NamedSeries struct {
	Label  string
	Points []metric.Point
}

// в app.css заведено ровно 8 пар классов палитры — девятая серия и далее
// молча отбрасываются.
const maxMultiSeries = 8

// обобщение metricSeriesSVG на несколько рядов — та же логика NaN-разрывов
// и порогов, но цвет линии по классу палитры.
func multiSeriesSVG(ctx context.Context, series []NamedSeries, unit string, thresholds []metricThreshold, deploys []deploy.Deployment, w, h int) templ.Component {
	return templ.Raw(multiSeriesMarkup(ctx, series, unit, thresholds, deploys, w, h))
}

func multiSeriesMarkup(ctx context.Context, series []NamedSeries, unit string, thresholds []metricThreshold, deploys []deploy.Deployment, w, h int) string {
	if len(series) > maxMultiSeries {
		series = series[:maxMultiSeries]
	}

	g := newChartGeom(w, h, 58, 16, 12, 26)

	// шкала растёт от нуля — метрики хоста (CPU/память/диск/сеть)
	// неотрицательны по природе, отдельный нижний домен не нужен.
	var max float64
	haveData := false
	longest := -1
	for si, s := range series {
		for _, p := range s.Points {
			if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
				continue
			}
			haveData = true
			if p.V > max {
				max = p.V
			}
		}
		if longest < 0 || len(s.Points) > len(series[longest].Points) {
			longest = si
		}
	}
	for _, t := range thresholds {
		if t.Value > max {
			max = t.Value
		}
	}

	var sb strings.Builder
	sb.WriteString(svgRoot("metric-chart", w, h, i18n.T(ctx, "a11y.chart.metric_multi")))

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

	scale := newYScaleFloat(max, 3)
	yLabel := func(v float64) string { return formatAxisValue(v, unit) }
	g = g.fitYLabels(scale, yLabel)

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	writeYGrid(&sb, g, scale, yLabel)

	// ось X — по самому длинному ряду: на практике все ряды на одной сетке
	// времени, это просто самый информативный вариант.
	times := make([]time.Time, len(series[longest].Points))
	for i, p := range series[longest].Points {
		times[i] = p.T
	}
	n := len(times)
	writeXTicks(&sb, g, timeAxis(times, func(i int) float64 { return g.xForIndex(i, n) }, 70))
	sb.WriteString(`</g>`)

	for _, t := range thresholds {
		yv := scale.yFor(g, t.Value)
		if yv < g.y0 || yv > g.y1 {
			continue
		}
		sb.WriteString(`<g class="chart-threshold"><line x1="`)
		sb.WriteString(formatCoord(g.x0))
		sb.WriteString(`" y1="`)
		sb.WriteString(formatCoord(yv))
		sb.WriteString(`" x2="`)
		sb.WriteString(formatCoord(g.x1))
		sb.WriteString(`" y2="`)
		sb.WriteString(formatCoord(yv))
		sb.WriteString(`" stroke="currentColor" stroke-width="1" stroke-dasharray="4 3"/><text x="`)
		sb.WriteString(formatCoord(g.x1 - 4))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(yv - 4))
		sb.WriteString(`" text-anchor="end" fill="currentColor">`)
		sb.WriteString(html.EscapeString(comparatorSymbol(t.Comparator) + " " + formatAxisValue(t.Value, unit)))
		sb.WriteString(`</text></g>`)
	}

	// без заливки под линией — area у 8 перекрывающихся рядов читалась бы
	// мутным пятном, не сериями.
	for i, s := range series {
		sn := len(s.Points)
		pts := make([]seriesPoint, sn)
		for j, p := range s.Points {
			x := g.xForIndex(j, sn)
			if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
				pts[j] = seriesPoint{x: x, has: false}
				continue
			}
			pts[j] = seriesPoint{x: x, y: scale.yFor(g, p.V), has: true}
		}
		class := "series-m" + strconv.Itoa(i+1)
		writeLineWithArea(&sb, pts, g.y1, "", "", `class="`+class+`"`)
	}

	// одна полоса на индекс, подсказка перечисляет все ряды с данными в
	// корзине — иначе пришлось бы наводиться на каждый ряд отдельно.
	band := (g.x1 - g.x0) / float64(n)
	for i := 0; i < n; i++ {
		var parts []string
		for _, s := range series {
			if i >= len(s.Points) {
				continue
			}
			p := s.Points[i]
			if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
				continue
			}
			parts = append(parts, s.Label+": "+formatAxisValue(p.V, unit))
		}
		if len(parts) == 0 {
			continue
		}
		// humanize.Time, не свой .Format — TestNoRawTimeFormattingOutsideHumanize
		// держит потолок литеральных макетов, новый вызов поднял бы его без нужды.
		writeHoverBand(&sb, g, g.xForIndex(i, n)-band/2, band,
			humanize.Time(ctx, times[i], time.UTC)+" — "+strings.Join(parts, " · "))
	}

	// Маркеры деплоев (C5): times уже построены по самому длинному ряду выше.
	writeDeployMarker(&sb, g, times, deploys)

	sb.WriteString(`</svg>`)
	return sb.String()
}

func axisLine(sb *strings.Builder, x1, y1v, x2, y2 float64) {
	sb.WriteString(`<line x1="`)
	sb.WriteString(formatCoord(x1))
	sb.WriteString(`" y1="`)
	sb.WriteString(formatCoord(y1v))
	sb.WriteString(`" x2="`)
	sb.WriteString(formatCoord(x2))
	sb.WriteString(`" y2="`)
	sb.WriteString(formatCoord(y2))
	sb.WriteString(`" stroke="currentColor" stroke-width="0.5" stroke-opacity="0.5"/>`)
}

type seriesPoint struct {
	x, y float64
	has  bool
}

// порог адаптивный (медиана шага ряда × 1.5) — соединяет только реальные
// соседние точки, не досочиняет данные; длинный пропуск остаётся разрывом.
func bridgeSparseGaps(pts []seriesPoint) []seriesPoint {
	idx := make([]int, 0, len(pts))
	for i, p := range pts {
		if p.has {
			idx = append(idx, i)
		}
	}
	if len(idx) < 3 {
		return pts
	}
	gaps := make([]int, 0, len(idx)-1)
	for i := 0; i+1 < len(idx); i++ {
		gaps = append(gaps, idx[i+1]-idx[i])
	}
	sort.Ints(gaps)
	median := gaps[len(gaps)/2]
	if median < 2 {
		return pts
	}
	limit := median * 3 / 2

	out := make([]seriesPoint, 0, len(pts))
	prev := -1
	for i, p := range pts {
		if !p.has {
			continue
		}
		if prev >= 0 && i-prev > limit {
			// одной пустой точки достаточно, чтобы сегмент оборвался.
			out = append(out, seriesPoint{})
		}
		out = append(out, p)
		prev = i
	}
	return out
}

// прямые отрезки, не сплайн — сглаживание рисовало бы значения между
// точками, которых не было. gradID должен быть уникален на странице.
var gradSeq atomic.Uint64

func uniqueGradID(base string) string {
	return base + "-" + strconv.FormatUint(gradSeq.Add(1), 36)
}

func writeLineWithArea(sb *strings.Builder, pts []seriesPoint, baseline float64, fillHex, gradID, lineAttr string) {
	gradID = uniqueGradID(gradID)
	if fillHex != "" {
		sb.WriteString(`<defs><linearGradient id="`)
		sb.WriteString(gradID)
		sb.WriteString(`" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="`)
		sb.WriteString(fillHex)
		sb.WriteString(`" stop-opacity="0.26"/><stop offset="1" stop-color="`)
		sb.WriteString(fillHex)
		sb.WriteString(`" stop-opacity="0"/></linearGradient></defs>`)
	}

	// одиночную корзину нельзя нарисовать полилинией (нужны две точки) —
	// рисуем короткую отметку, а не пропускаем: всплеск в тишине важен.
	markW := 2.0
	if len(pts) > 1 {
		if step := (pts[len(pts)-1].x - pts[0].x) / float64(len(pts)-1); step > 0 {
			markW = step * 0.6
		}
	}

	pts = bridgeSparseGaps(pts)

	for i := 0; i < len(pts); {
		if !pts[i].has {
			i++
			continue
		}
		j := i
		for j < len(pts) && pts[j].has {
			j++
		}
		seg := pts[i:j]
		i = j
		if len(seg) == 1 {
			// рисуется теми же атрибутами штриха, что и линия — совпадает с
			// ней по цвету и теме.
			pt := seg[0]
			x0, x1 := pt.x-markW/2, pt.x+markW/2
			if fillHex != "" {
				sb.WriteString(`<path fill="url(#`)
				sb.WriteString(gradID)
				sb.WriteString(`)" stroke="none" d="M`)
				sb.WriteString(formatCoord(x0))
				sb.WriteByte(' ')
				sb.WriteString(formatCoord(baseline))
				sb.WriteString(`L`)
				sb.WriteString(formatCoord(x0))
				sb.WriteByte(' ')
				sb.WriteString(formatCoord(pt.y))
				sb.WriteString(`L`)
				sb.WriteString(formatCoord(x1))
				sb.WriteByte(' ')
				sb.WriteString(formatCoord(pt.y))
				sb.WriteString(`L`)
				sb.WriteString(formatCoord(x1))
				sb.WriteByte(' ')
				sb.WriteString(formatCoord(baseline))
				sb.WriteString(`Z"/>`)
			}
			sb.WriteString(`<polyline points="`)
			sb.WriteString(formatCoord(x0))
			sb.WriteByte(',')
			sb.WriteString(formatCoord(pt.y))
			sb.WriteByte(' ')
			sb.WriteString(formatCoord(x1))
			sb.WriteByte(',')
			sb.WriteString(formatCoord(pt.y))
			sb.WriteString(`" fill="none" `)
			sb.WriteString(lineAttr)
			sb.WriteString(` stroke-width="1.5"/>`)
			continue
		}
		if fillHex != "" {
			sb.WriteString(`<path fill="url(#`)
			sb.WriteString(gradID)
			sb.WriteString(`)" stroke="none" d="M`)
			sb.WriteString(formatCoord(seg[0].x))
			sb.WriteByte(' ')
			sb.WriteString(formatCoord(baseline))
			for _, p := range seg {
				sb.WriteString(`L`)
				sb.WriteString(formatCoord(p.x))
				sb.WriteByte(' ')
				sb.WriteString(formatCoord(p.y))
			}
			sb.WriteString(`L`)
			sb.WriteString(formatCoord(seg[len(seg)-1].x))
			sb.WriteByte(' ')
			sb.WriteString(formatCoord(baseline))
			sb.WriteString(`Z"/>`)
		}
		sb.WriteString(`<polyline points="`)
		for k, p := range seg {
			if k > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(formatCoord(p.x))
			sb.WriteByte(',')
			sb.WriteString(formatCoord(p.y))
		}
		sb.WriteString(`" fill="none" `)
		sb.WriteString(lineAttr)
		sb.WriteString(` stroke-width="1.5"/>`)
	}
}

func comparatorSymbol(cmp string) string {
	if cmp == "lt" {
		return "<"
	}
	return ">"
}

// CompactNumber — тот же формат, что в таблицах правил метрик.
func formatAxisValue(v float64, unit string) string {
	s := humanize.CompactNumber(v)
	// "1" — юнит безразмерной метрики по OTLP — печатать на оси нельзя,
	// «17 1» читается как число, не как «17 штук».
	if unit != "" && unit != "1" {
		s += " " + unit
	}
	return s
}

func metricTimeLabel(t time.Time, spanHours float64) string {
	t = t.UTC()
	if spanHours >= 48 {
		return t.Format("02.01")
	}
	return t.Format("15:04")
}

const (
	sparklineWidth  = 96
	sparklineHeight = 24
)

// buckets — числа из event.Query.Sparklines (посчитаны CH), HTML-экранирование
// не нужно — templ.Raw здесь безопасен.
func sparklineSVG(ctx context.Context, buckets []uint64, w, h int, format func(uint64) string) templ.Component {
	return templ.Raw(sparklinePolyline(ctx, buckets, w, h, format))
}

func sparklinePolyline(ctx context.Context, buckets []uint64, w, h int, format func(uint64) string) string {
	var max uint64
	for _, v := range buckets {
		if v > max {
			max = v
		}
	}
	if len(buckets) == 0 || max == 0 {
		return flatlineSVG(ctx, w, h)
	}

	n := len(buckets)
	linePts := make([]seriesPoint, n)
	for i, v := range buckets {
		var x float64
		if n > 1 {
			x = float64(i) / float64(n-1) * float64(w)
		}
		linePts[i] = seriesPoint{x: x, y: float64(h) - float64(v)/float64(max)*float64(h), has: true}
	}

	var sb strings.Builder
	sb.WriteString(svgRoot("sparkline", w, h, i18n.T(ctx, "a11y.chart.sparkline")))
	if len(buckets) > 0 {
		if format == nil {
			format = func(v uint64) string { return strconv.FormatUint(v, 10) }
		}
		lo, hi := buckets[0], buckets[0]
		for _, v := range buckets {
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		sb.WriteString(`<title>`)
		sb.WriteString(html.EscapeString("min " + format(lo) + " · max " + format(hi) +
			" · " + format(buckets[len(buckets)-1])))
		sb.WriteString(`</title>`)
	}
	writeLineWithArea(&sb, linePts, float64(h), "currentColor", "gradSpark", `stroke="currentColor"`)
	sb.WriteString(`</svg>`)
	return sb.String()
}

func flatlineSVG(ctx context.Context, w, h int) string {
	// по базовой линии, не по середине — середина читалась бы как реальное
	// значение выше настоящих нулей.
	y := formatCoord(float64(h) - 0.5)
	var sb strings.Builder
	sb.WriteString(svgRoot("sparkline", w, h, i18n.T(ctx, "a11y.chart.sparkline_empty")))
	sb.WriteString(`<polyline points="0,`)
	sb.WriteString(y)
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(',')
	sb.WriteString(y)
	sb.WriteString(`" fill="none" stroke="currentColor" stroke-width="1.5"/></svg>`)
	return sb.String()
}

func formatCoord(f float64) string {
	// нефинитное значение дало бы SVG-атрибут "NaN"/"+Inf" — CH теоретически
	// может прислать такое, хотя пороги уже отсекаются на входе.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "0.0"
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}

const (
	perfSparklineWidth  = 96
	perfSparklineHeight = 24
)

// переиспользует sparklineSVG — числа из trace.Query.EndpointLatency,
// templ.Raw остаётся безопасным.
func latencySparklineSVG(ctx context.Context, points []trace.LatencyPoint, w, h int) templ.Component {
	vals := make([]uint64, len(points))
	for i, p := range points {
		vals[i] = uint64(p.P95)
	}
	// Значения — микросекунды: в подсказке приводим к ms/s, как на осях.
	return sparklineSVG(ctx, vals, w, h, func(v uint64) string { return formatUSAxis(float64(v)) })
}

const (
	perfLatencyChartWidth  = 1200
	perfLatencyChartHeight = 220
)

// захардкожены, не currentColor — нужны два разных цвета линий в одном SVG;
// сам цвет — из app.css по токенам.
var perfLatencyLineClasses = [2]string{"series-p50", "series-p95"}

// пустой ряд → flatlineSVG, тем же принципом «нет данных ≠ ошибка рендера».
func latencyLinesSVG(ctx context.Context, points []trace.LatencyPoint, deploys []deploy.Deployment, w, h int) templ.Component {
	return templ.Raw(latencyLinesMarkup(ctx, points, deploys, w, h))
}

func latencyLinesMarkup(ctx context.Context, points []trace.LatencyPoint, deploys []deploy.Deployment, w, h int) string {
	var max uint32
	for _, p := range points {
		if p.P95 > max {
			max = p.P95
		}
	}
	if len(points) == 0 || max == 0 {
		return flatlineSVG(ctx, w, h)
	}

	g := newChartGeom(w, h, 64, 16, 26, 26)
	scale := newYScaleFloat(float64(max), 3)
	g = g.fitYLabels(scale, formatUSAxis)
	n := len(points)

	var sb strings.Builder
	sb.WriteString(svgRoot("latency-chart", w, h, i18n.T(ctx, "a11y.chart.latency")))

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	writeYGrid(&sb, g, scale, formatUSAxis)
	times := make([]time.Time, n)
	for i, p := range points {
		times[i] = p.T
	}
	writeXTicks(&sb, g, timeAxis(times, func(i int) float64 { return g.xForIndex(i, n) }, 70))
	sb.WriteString(`</g>`)

	// p50 с заливкой, p95 только линией — заливка обеих дала бы мутное
	// наложение; пустые корзины (Count==0) — разрыв, не провал в ноль.
	p50pts := make([]seriesPoint, n)
	p95pts := make([]seriesPoint, n)
	for i, p := range points {
		x := g.xForIndex(i, n)
		has := p.Count > 0
		p50pts[i] = seriesPoint{x: x, y: scale.yFor(g, float64(p.P50)), has: has}
		p95pts[i] = seriesPoint{x: x, y: scale.yFor(g, float64(p.P95)), has: has}
	}
	writeLineWithArea(&sb, p50pts, g.y1, "currentColor", "gradLatP50", `class="`+perfLatencyLineClasses[0]+`"`)
	writeLineWithArea(&sb, p95pts, g.y1, "", "", `class="`+perfLatencyLineClasses[1]+`"`)

	band := (g.x1 - g.x0) / float64(n)
	for i, p := range points {
		writeHoverBand(&sb, g, g.xForIndex(i, n)-band/2, band,
			humanize.Time(ctx, p.T, time.UTC)+" · p50 "+formatUSAxis(float64(p.P50))+
				" · p95 "+formatUSAxis(float64(p.P95))+" · "+
				i18n.Tn(ctx, "chart.bar.transactions", int(p.Count)))
	}

	writeDeployMarker(&sb, g, times, deploys)

	sb.WriteString(`</svg>`)
	return sb.String()
}

func throughputBarsSVG(ctx context.Context, points []trace.LatencyPoint, deploys []deploy.Deployment, w, h int) templ.Component {
	return templ.Raw(throughputBarsMarkup(ctx, points, deploys, w, h))
}

func throughputBarsMarkup(ctx context.Context, points []trace.LatencyPoint, deploys []deploy.Deployment, w, h int) string {
	var max uint64
	for _, p := range points {
		if p.Count > max {
			max = p.Count
		}
	}
	// пустая и заполненная версии графика обязаны называться одинаково —
	// подпись про пропускную способность, не про задержку или частоту событий.
	if len(points) == 0 || max == 0 {
		return chartEmptyAxis(w, h, i18n.T(ctx, "a11y.chart.throughput"))
	}

	g := newChartGeom(w, h, 48, 16, 26, 26)
	scale := newYScale(max, 3)
	g = g.fitYLabels(scale, formatCountAxis)
	n := len(points)
	barW := g.barWidth(n)
	gap := barW * 0.15

	var sb strings.Builder
	sb.WriteString(svgRoot("chart-freq", w, h, i18n.T(ctx, "a11y.chart.throughput")))

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	writeYGrid(&sb, g, scale, formatCountAxis)
	times := make([]time.Time, n)
	for i, p := range points {
		times[i] = p.T
	}
	writeXTicks(&sb, g, timeAxis(times, func(i int) float64 { return g.x0 + float64(i)*barW }, 70))
	sb.WriteString(`</g>`)

	for i, p := range points {
		y := scale.yFor(g, float64(p.Count))
		sb.WriteString(`<rect x="`)
		sb.WriteString(formatCoord(g.x0 + float64(i)*barW + gap/2))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(y))
		sb.WriteString(`" width="`)
		sb.WriteString(formatCoord(barW - gap))
		sb.WriteString(`" height="`)
		sb.WriteString(formatCoord(g.y1 - y))
		sb.WriteString(`" fill="currentColor"><title>`)
		sb.WriteString(html.EscapeString(humanize.Time(ctx, p.T, time.UTC) + " — " +
			i18n.Tn(ctx, "chart.bar.transactions", int(p.Count))))
		sb.WriteString(`</title></rect>`)
	}

	// столбчатая шкала ставит точку i в g.x0+i*barW — маркеру нужна та же
	// шкала, копия g с укороченным x1.
	bg := g
	bg.x1 = g.x0 + float64(n-1)*barW
	writeDeployMarker(&sb, bg, times, deploys)

	sb.WriteString(`</svg>`)
	return sb.String()
}

func durationHistogramSVG(ctx context.Context, buckets []trace.DurationBucket, w, h int) templ.Component {
	return templ.Raw(durationHistogramMarkup(ctx, buckets, w, h))
}

func durationHistogramMarkup(ctx context.Context, buckets []trace.DurationBucket, w, h int) string {
	var max uint64
	for _, b := range buckets {
		if b.Count > max {
			max = b.Count
		}
	}
	// гистограмма распределения — не временной ряд, со своим ключом a11y.chart.histogram.
	if len(buckets) == 0 || max == 0 {
		return chartEmptyAxis(w, h, i18n.T(ctx, "a11y.chart.histogram"))
	}

	g := newChartGeom(w, h, 48, 16, 26, 26)
	scale := newYScale(max, 3)
	g = g.fitYLabels(scale, formatCountAxis)
	n := len(buckets)
	barW := g.barWidth(n)
	gap := barW * 0.15

	var sb strings.Builder
	sb.WriteString(svgRoot("chart-freq", w, h, i18n.T(ctx, "a11y.chart.histogram")))

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	writeYGrid(&sb, g, scale, formatCountAxis)
	// не каждая граница подписана — их до двадцати, подписи наезжали бы.
	lastX := -1e9
	var ticks []xTick
	for i, b := range buckets {
		x := g.x0 + float64(i+1)*barW
		if x-lastX < 70 {
			continue
		}
		lastX = x
		ticks = append(ticks, xTick{x: x, text: formatUSAxis(float64(b.UpperUS))})
	}
	writeXTicks(&sb, g, ticks)
	sb.WriteString(`</g>`)

	for i, b := range buckets {
		y := scale.yFor(g, float64(b.Count))
		lower := "0"
		if i > 0 {
			lower = formatUSAxis(float64(buckets[i-1].UpperUS))
		}
		sb.WriteString(`<rect x="`)
		sb.WriteString(formatCoord(g.x0 + float64(i)*barW + gap/2))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(y))
		sb.WriteString(`" width="`)
		sb.WriteString(formatCoord(barW - gap))
		sb.WriteString(`" height="`)
		sb.WriteString(formatCoord(g.y1 - y))
		sb.WriteString(`" fill="currentColor"><title>`)
		sb.WriteString(html.EscapeString(lower + "–" + formatUSAxis(float64(b.UpperUS)) + " — " +
			i18n.Tn(ctx, "chart.bar.transactions", int(b.Count))))
		sb.WriteString(`</title></rect>`)
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}

const (
	chartWidth  = 1200
	chartHeight = 180
)

const (
	chartPadL = 40
	chartPadR = 10
	chartPadT = 10
	chartPadB = 22
)

// пустые данные рисуют плоскую ось у нижнего края, тем же принципом, что
// flatlineSVG. points из event.Query.Series (CH) — templ.Raw безопасен.
func chartSVG(ctx context.Context, points []event.Point, w, h int) templ.Component {
	return templ.Raw(chartBars(ctx, points, w, h))
}

// иначе подписи выходят вида 37/74/111 — формально верные, но прикинуть
// значение нельзя.
func niceStep(max uint64, targetLines int) uint64 {
	if max == 0 || targetLines <= 0 {
		return 1
	}
	raw := float64(max) / float64(targetLines)
	if raw < 1 {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if step := m * mag; step >= raw {
			return uint64(step)
		}
	}
	return uint64(10 * mag)
}

// тот же ряд 1/2/5×10ⁿ, но без округления шага до целого. Шаг всегда строго
// положителен и конечен — на субнормалях raw/mag могут округлиться в 0.
func niceStepFloat(max float64, targetLines int) float64 {
	if max <= 0 || math.IsNaN(max) || math.IsInf(max, 0) || targetLines <= 0 {
		return 1
	}
	raw := max / float64(targetLines)
	if raw <= 0 {
		return max
	}
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	if mag <= 0 {
		return raw
	}
	for _, m := range []float64{1, 2, 5, 10} {
		if step := m * mag; step >= raw {
			return step
		}
	}
	return 10 * mag
}

func chartBars(ctx context.Context, points []event.Point, w, h int) string {
	x0, x1 := float64(chartPadL), float64(w-chartPadR)
	y0, y1 := float64(chartPadT), float64(h-chartPadB)

	var max uint64
	for _, p := range points {
		if p.N > max {
			max = p.N
		}
	}
	// считается до осей: левое поле растёт под самую широкую подпись
	// (yAxisPadL), ось встаёт уже на сдвинутый x0.
	var step, top uint64
	if max > 0 {
		step = niceStep(max, 3)
		top = (max/step + 1) * step
		var labels []string
		for v := uint64(0); v <= top; v += step {
			labels = append(labels, strconv.FormatUint(v, 10))
		}
		x0 = yAxisPadL(w, x0, labels)
	}

	var sb strings.Builder
	// preserveAspectRatio по умолчанию — неравномерное растяжение растянуло
	// бы и подписи; под широкую карточку увеличены сами chartWidth/chartHeight.
	sb.WriteString(svgRoot("chart-freq", w, h, i18n.T(ctx, "a11y.chart.frequency")))

	sb.WriteString(`<g class="chart-axis">`)
	axisLine(&sb, x0, y0, x0, y1)
	axisLine(&sb, x0, y1, x1, y1)

	if len(points) == 0 || max == 0 {
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(x0 - 6))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(y1))
		sb.WriteString(`" text-anchor="end" dominant-baseline="middle" fill="currentColor">0</text></g></svg>`)
		return sb.String()
	}

	// верх строго выше максимума — иначе столбик упирается в рамку и график
	// читается забором; запас сверху задаёт «шапку», по которой виден пик.
	yFor := func(v uint64) float64 {
		return y1 - float64(v)/float64(top)*(y1-y0)
	}
	for v := uint64(0); v <= top; v += step {
		yv := yFor(v)
		if v > 0 {
			axisLine(&sb, x0, yv, x1, yv)
		}
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(x0 - 6))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(yv))
		sb.WriteString(`" text-anchor="end" dominant-baseline="middle" fill="currentColor">`)
		sb.WriteString(strconv.FormatUint(v, 10))
		sb.WriteString(`</text>`)
	}

	// подписи — по границам суток, не каждой корзине; целимся в
	// targetDayLabels равномерных через шаг k, чтобы ось «дышала» на любом окне.
	n := len(points)
	barW := (x1 - x0) / float64(n)
	const targetDayLabels = 7
	var dayIdx []int
	for i, p := range points {
		if i == 0 || p.T.UTC().YearDay() != points[i-1].T.UTC().YearDay() {
			dayIdx = append(dayIdx, i)
		}
	}
	k := (len(dayIdx) + targetDayLabels - 1) / targetDayLabels
	if k < 1 {
		k = 1
	}
	prevDayLabelRight := math.Inf(-1)
	for j, idx := range dayIdx {
		if j%k != 0 {
			continue
		}
		x := x0 + float64(idx)*barW
		if idx > 0 {
			axisLine(&sb, x, y0, x, y1)
		}
		text := points[idx].T.UTC().Format("02.01")
		// якорь и защита от наезда — та же логика xLabelPlacement, что у
		// writeXTicks; draw=false, если наезд не удалось починить сменой якоря.
		anchor, _, right, draw := xLabelPlacement(w, x0, x1, prevDayLabelRight, x, text)
		if !draw {
			continue
		}
		prevDayLabelRight = right
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(x))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(float64(h) - 7))
		sb.WriteString(`" text-anchor="` + anchor + `" fill="currentColor">`)
		sb.WriteString(html.EscapeString(text))
		sb.WriteString(`</text>`)
	}
	sb.WriteString(`</g>`)

	// title — нативная подсказка браузера, без JS, переживает его отключение.
	gap := barW * 0.15
	for i, p := range points {
		barH := float64(p.N) / float64(top) * (y1 - y0)
		x := x0 + float64(i)*barW + gap/2
		y := y1 - barH
		sb.WriteString(`<rect x="`)
		sb.WriteString(formatCoord(x))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(y))
		sb.WriteString(`" width="`)
		sb.WriteString(formatCoord(barW - gap))
		sb.WriteString(`" height="`)
		sb.WriteString(formatCoord(barH))
		sb.WriteString(`" fill="currentColor"><title>`)
		sb.WriteString(html.EscapeString(humanize.Time(ctx, p.T, time.UTC)))
		sb.WriteString(` — `)
		sb.WriteString(html.EscapeString(i18n.Tn(ctx, "chart.bar.events", int(p.N))))
		sb.WriteString(`</title></rect>`)
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}

// заглушка на три разных графика (задержка, throughput, Web Vital) — label
// передаёт вызывающий, зашитое имя одного было бы враньём для остальных.
func chartEmptyAxis(w, h int, label string) string {
	y := formatCoord(float64(h) - 0.5)
	var sb strings.Builder
	sb.WriteString(strings.TrimSuffix(svgRoot("chart", w, h, label), ">"))
	sb.WriteString(` preserveAspectRatio="none"><line x1="0" y1="`)
	sb.WriteString(y)
	sb.WriteString(`" x2="`)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteString(`" y2="`)
	sb.WriteString(y)
	sb.WriteString(`" stroke="currentColor" stroke-width="1"/></svg>`)
	return sb.String()
}

const (
	availabilityBarsWidth  = 192
	availabilityBarsHeight = 24
)

// цвет — из app.css по классу, не currentColor: нужны разные цвета в одном SVG.
const (
	availabilityClassUp      = "bar-up"
	availabilityClassPartial = "bar-partial"
	availabilityClassDown    = "bar-down"
	availabilityClassEmpty   = "bar-empty"
)

// пустой bars рисует один серый прямоугольник — тот же принцип, что у
// flatlineSVG/chartEmptyAxis; templ.Raw безопасен (bars — числа).
func availabilityBarsSVG(ctx context.Context, bars []uptime.UptimeStat, w, h int) templ.Component {
	return templ.Raw(availabilityBarsMarkup(ctx, bars, w, h))
}

func availabilityBarsMarkup(ctx context.Context, bars []uptime.UptimeStat, w, h int) string {
	if len(bars) == 0 {
		return availabilityEmptyBarsSVG(ctx, w, h)
	}

	n := len(bars)
	barW := float64(w) / float64(n)
	gap := barW * 0.1

	var rects strings.Builder
	for i, b := range bars {
		x := float64(i)*barW + gap/2
		rects.WriteString(`<rect x="`)
		rects.WriteString(formatCoord(x))
		rects.WriteString(`" y="0" width="`)
		rects.WriteString(formatCoord(barW - gap))
		rects.WriteString(`" height="`)
		rects.WriteString(strconv.Itoa(h))
		rects.WriteString(`" class="`)
		rects.WriteString(availabilityBarClass(b))
		rects.WriteString(`"><title>`)
		rects.WriteString(html.EscapeString(availabilityBarLabel(ctx, b)))
		rects.WriteString(`</title></rect>`)
	}

	var sb strings.Builder
	// object-fit не работает на инлайновом корневом <svg> — растягивать
	// умеет только сам preserveAspectRatio, и только у этого графика.
	sb.WriteString(strings.TrimSuffix(svgRoot("availability-bars", w, h, i18n.T(ctx, "a11y.chart.availability")), ">"))
	sb.WriteString(` preserveAspectRatio="none">`)
	sb.WriteString(rects.String())
	sb.WriteString(`</svg>`)
	return sb.String()
}

func availabilityBarClass(b uptime.UptimeStat) string {
	switch {
	case b.Total == 0:
		return availabilityClassEmpty
	case b.OK == b.Total:
		return availabilityClassUp
	case b.OK*2 >= b.Total:
		// Большинство проверок успешно, но были и сбои — «постреливает».
		// Целочисленно (OK*2 >= Total) == доля успехов >= 50%, без float.
		return availabilityClassPartial
	default:
		return availabilityClassDown
	}
}

// класс и подпись выводятся из availabilityBarClass через одну таблицу —
// разъехаться им больше не из чего.
var availabilityBarLabelKey = map[string]string{
	availabilityClassUp:      "chart.bar.up",
	availabilityClassPartial: "chart.bar.partial",
	availabilityClassDown:    "chart.bar.down",
	availabilityClassEmpty:   "chart.no_data",
}

// цвет — единственный сигнал состояния в SVG, без title screen reader/hover
// ничего не получают; текст из каталога экранируется вызывающей стороной.
func availabilityBarLabel(ctx context.Context, b uptime.UptimeStat) string {
	return i18n.T(ctx, availabilityBarLabelKey[availabilityBarClass(b)])
}

func availabilityEmptyBarsSVG(ctx context.Context, w, h int) string {
	var sb strings.Builder
	// тот же фикс, что у availabilityBarsMarkup — пустое состояние
	// растягивается одинаково с заполненным.
	sb.WriteString(strings.TrimSuffix(svgRoot("availability-bars", w, h, i18n.T(ctx, "a11y.chart.availability")), ">"))
	sb.WriteString(` preserveAspectRatio="none">`)
	sb.WriteString(`<rect x="0" y="0" width="`)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteString(`" height="`)
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" class="`)
	sb.WriteString(availabilityClassEmpty)
	sb.WriteString(`"/></svg>`)
	return sb.String()
}

// waterfallMaxRows — трейс из тысяч спанов не должен родить чудовищный SVG;
// рисуем первые N в порядке обхода, страница сообщает об усечении.
const (
	waterfallWidth   = 900
	waterfallRowH    = 18
	waterfallLabelW  = 300
	waterfallPadX    = 4
	waterfallIndent  = 12
	waterfallMaxRows = 200
)

// цвет — из app.css по классу, не currentColor: нужны два цвета в одном SVG.
const (
	waterfallClassOK    = "wf-ok"
	waterfallClassError = "wf-err"
)

// op/description — недоверенные данные, экранируются (templ.EscapeString) —
// единственный SVG-хелпер, куда попадают строки пользователя, не только числа.
func waterfallSVG(ctx context.Context, spans []trace.SpanRow, errIssues map[string]int64, totalUS uint32, w int) templ.Component {
	return templ.Raw(waterfallMarkup(ctx, spans, errIssues, totalUS, w))
}

func waterfallMarkup(ctx context.Context, spans []trace.SpanRow, errIssues map[string]int64, totalUS uint32, w int) string {
	ordered := orderSpanTree(spans, waterfallMaxRows)
	if len(ordered) == 0 {
		return ""
	}
	if totalUS == 0 {
		totalUS = 1
	}

	barX0 := waterfallLabelW
	barAreaW := float64(w - waterfallLabelW - waterfallPadX)
	if barAreaW < 1 {
		barAreaW = 1
	}
	h := len(ordered) * waterfallRowH

	var b strings.Builder
	b.WriteString(strings.TrimSuffix(svgRoot("waterfall", w, h, i18n.T(ctx, "a11y.chart.waterfall")), ">"))
	b.WriteString(` font-family="monospace" font-size="10">`)

	for i, os := range ordered {
		s := os.span
		y := float64(i * waterfallRowH)
		barH := float64(waterfallRowH - 4)

		x := float64(barX0) + float64(s.StartUS)/float64(totalUS)*barAreaW
		bw := float64(s.DurationUS) / float64(totalUS) * barAreaW
		if bw < 1 {
			bw = 1
		}

		issueID, isErr := errIssues[s.SpanID]
		cls := waterfallClassOK
		if isErr || (s.Status != "" && s.Status != "ok") {
			cls = waterfallClassError
		}

		if isErr {
			b.WriteString(`<a href="/issues/`)
			b.WriteString(strconv.FormatInt(issueID, 10))
			b.WriteString(`">`)
		}

		full := waterfallLabel(s)
		b.WriteString(`<rect x="`)
		b.WriteString(formatCoord(x))
		b.WriteString(`" y="`)
		b.WriteString(formatCoord(y + 2))
		b.WriteString(`" width="`)
		b.WriteString(formatCoord(bw))
		b.WriteString(`" height="`)
		b.WriteString(formatCoord(barH))
		b.WriteString(`" class="`)
		b.WriteString(cls)
		b.WriteString(`"><title>`)
		b.WriteString(html.EscapeString(full))
		b.WriteString(`</title></rect>`)

		labelX := waterfallPadX + os.depth*waterfallIndent
		if label := fitWaterfallLabel(full, float64(waterfallLabelW-waterfallPadX-labelX)); label != "" {
			b.WriteString(`<text x="`)
			b.WriteString(strconv.Itoa(labelX))
			b.WriteString(`" y="`)
			b.WriteString(formatCoord(y + float64(waterfallRowH) - 5))
			b.WriteString(`" class="waterfall-label">`)
			b.WriteString(templ.EscapeString(label))
			b.WriteString(`</text>`)
		}

		if isErr {
			b.WriteString(`</a>`)
		}
	}

	b.WriteString(`</svg>`)
	return b.String()
}

type orderedSpan struct {
	span  trace.SpanRow
	depth int
}

// корни в исходном порядке (спаны уже отсортированы по времени), дети
// рекурсивно; циклы обрезаются посещением.
func orderSpanTree(spans []trace.SpanRow, max int) []orderedSpan {
	if len(spans) == 0 {
		return nil
	}
	present := make(map[string]bool, len(spans))
	for _, s := range spans {
		if s.SpanID != "" {
			present[s.SpanID] = true
		}
	}
	children := make(map[string][]trace.SpanRow)
	var roots []trace.SpanRow
	for _, s := range spans {
		if s.ParentSpanID == "" || !present[s.ParentSpanID] {
			roots = append(roots, s)
			continue
		}
		children[s.ParentSpanID] = append(children[s.ParentSpanID], s)
	}

	out := make([]orderedSpan, 0, len(spans))
	visited := make(map[string]bool, len(spans))
	var walk func(s trace.SpanRow, depth int)
	walk = func(s trace.SpanRow, depth int) {
		if len(out) >= max {
			return
		}
		if s.SpanID != "" {
			if visited[s.SpanID] {
				return
			}
			visited[s.SpanID] = true
		}
		out = append(out, orderedSpan{span: s, depth: depth})
		for _, c := range children[s.SpanID] {
			if len(out) >= max {
				return
			}
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		if len(out) >= max {
			break
		}
		walk(r, 0)
	}
	return out
}

func waterfallLabel(s trace.SpanRow) string {
	op := s.Op
	if op == "" {
		op = s.Description
	}
	return op + " " + waterfallMS(s.DurationUS)
}

// .waterfall-label — фиксированный кегль (--fs-label, вне тиров chart-vbN),
// та же ширина руны, что и у флеймграфа; полный текст остаётся в <title>.
func fitWaterfallLabel(label string, avail float64) string {
	fit := int(avail / flameCharWidthPx)
	r := []rune(label)
	if len(r) <= fit {
		return label
	}
	if fit-1 < 3 {
		return ""
	}
	return string(r[:fit-1]) + "…"
}

// как formatDurationUS в templates, но локально — svg.go в другом пакете.
func waterfallMS(us uint32) string {
	switch {
	case us < 1000:
		return strconv.FormatUint(uint64(us), 10) + "µs"
	case us < 1_000_000:
		return strconv.FormatFloat(float64(us)/1000, 'f', 1, 64) + "ms"
	default:
		return strconv.FormatFloat(float64(us)/1_000_000, 'f', 2, 64) + "s"
	}
}

const (
	perfVitalChartWidth  = 240
	perfVitalChartHeight = 48
)

// пустой ряд → flatlineSVG. format приводит число к той же записи, что и
// рядом в строке (мс/с либо безразмерный CLS) — иначе подсказка была бы без единицы.
func vitalSeriesSVG(ctx context.Context, points []trace.VitalPoint, w, h int, format func(float64) string) templ.Component {
	return templ.Raw(vitalSeriesMarkup(ctx, points, w, h, format))
}

func vitalSeriesMarkup(ctx context.Context, points []trace.VitalPoint, w, h int, format func(float64) string) string {
	var max float64
	for _, p := range points {
		if p.P75 > max {
			max = p.P75
		}
	}
	if len(points) == 0 || max <= 0 {
		return flatlineSVG(ctx, w, h)
	}

	n := len(points)
	linePts := make([]seriesPoint, n)
	for i, p := range points {
		var x float64
		if n > 1 {
			x = float64(i) / float64(n-1) * float64(w)
		}
		linePts[i] = seriesPoint{x: x, y: float64(h) - p.P75/max*float64(h), has: true}
	}

	var sb strings.Builder
	sb.WriteString(svgRoot("vital-chart", w, h, i18n.T(ctx, "a11y.chart.vital")))
	if format != nil && len(points) > 0 {
		lo, hi := points[0].P75, points[0].P75
		for _, p := range points {
			if p.P75 < lo {
				lo = p.P75
			}
			if p.P75 > hi {
				hi = p.P75
			}
		}
		last := points[len(points)-1]
		sb.WriteString(`<title>`)
		sb.WriteString(html.EscapeString(
			humanize.Time(ctx, points[0].T, time.UTC) + " – " + humanize.Time(ctx, last.T, time.UTC) +
				" · min " + format(lo) + " · max " + format(hi) + " · " + format(last.P75)))
		sb.WriteString(`</title>`)
	}
	writeLineWithArea(&sb, linePts, float64(h), "currentColor", "gradVital", `stroke="currentColor"`)
	sb.WriteString(`</svg>`)
	return sb.String()
}

// latencyChartWidth/Height — размер stacked-bar-графика задержек на странице
// монитора.
const (
	latencyChartWidth  = 720
	latencyChartHeight = 160
)

// порядок укладки снизу вверх: DNS, connect, TLS, TTFB; цвет — из app.css
// по классу, не currentColor (нужны четыре цвета).
var latencySegmentClasses = [4]string{"seg-dns", "seg-connect", "seg-tls", "seg-ttfb"}

// красная, чтобы читаться как событие, не как обычная фаза.
const latencyCapClass = "seg-cap"

// технические названия одинаковы во всех языках — в каталог не выносятся.
var latencySegmentNames = [4]string{"DNS", "TCP", "TLS", "TTFB"}

// шкала — по максимуму СУММЫ фаз, не AvgTotalMs: час с таймаутом даёт
// total≈30000мс при фазах≈0, нормировка на total схлопнула бы здоровые часы.
func latencyStackedSVG(ctx context.Context, points []uptime.LatencyPoint, deploys []deploy.Deployment, w, h int) templ.Component {
	return templ.Raw(latencyStackedMarkup(ctx, points, deploys, w, h))
}

func latencyStackedMarkup(ctx context.Context, points []uptime.LatencyPoint, deploys []deploy.Deployment, w, h int) string {
	var maxPhase uint32
	for _, p := range points {
		if sum := p.AvgDNSMs + p.AvgConnectMs + p.AvgTLSMs + p.AvgTTFBMs; sum > maxPhase {
			maxPhase = sum
		}
	}
	if len(points) == 0 || maxPhase == 0 {
		return chartEmptyAxis(w, h, i18n.T(ctx, "a11y.chart.latency"))
	}

	g := newChartGeom(w, h, 48, 16, 26, 26)
	scale := newYScaleFloat(float64(maxPhase), 3)
	g = g.fitYLabels(scale, formatMsAxis)
	n := len(points)
	barW := g.barWidth(n)
	gap := barW * 0.15
	plotH := g.y1 - g.y0

	var sb strings.Builder
	sb.WriteString(svgRoot("latency-chart", w, h, i18n.T(ctx, "a11y.chart.latency")))

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	writeYGrid(&sb, g, scale, formatMsAxis)
	times := make([]time.Time, n)
	for i, p := range points {
		times[i] = p.T
	}
	writeXTicks(&sb, g, timeAxis(times, func(i int) float64 { return g.x0 + float64(i)*barW }, 70))
	sb.WriteString(`</g>`)

	for i, p := range points {
		slotX := g.x0 + float64(i)*barW + gap/2
		bw := barW - gap
		segments := [4]uint32{p.AvgDNSMs, p.AvgConnectMs, p.AvgTLSMs, p.AvgTTFBMs}
		// зазор обнажает фон карточки — фазы различаются не только цветом;
		// для очень тонких сегментов пропускается, иначе они исчезли бы.
		const segGap = 1.5
		bottom := g.y1
		for si, ms := range segments {
			if ms == 0 {
				continue
			}
			segH := float64(ms) / scale.top * plotH
			top := bottom - segH
			bottom = top
			drawY, drawH := top, segH
			if segH > segGap*2 {
				drawY, drawH = top+segGap, segH-segGap
			}
			sb.WriteString(`<rect x="`)
			sb.WriteString(formatCoord(slotX))
			sb.WriteString(`" y="`)
			sb.WriteString(formatCoord(drawY))
			sb.WriteString(`" width="`)
			sb.WriteString(formatCoord(bw))
			sb.WriteString(`" height="`)
			sb.WriteString(formatCoord(drawH))
			sb.WriteString(`" class="`)
			sb.WriteString(latencySegmentClasses[si])
			sb.WriteString(`"/>`)
		}

		// средний total выше видимой шкалы — треугольник у верхней рамки над слотом.
		capped := float64(p.AvgTotalMs) > scale.top
		if capped {
			cx := slotX + bw/2
			sb.WriteString(`<path class="`)
			sb.WriteString(latencyCapClass)
			sb.WriteString(`" d="M`)
			sb.WriteString(formatCoord(cx))
			sb.WriteByte(' ')
			sb.WriteString(formatCoord(g.y0))
			sb.WriteString(`l-4 7h8z"/>`)
		}

		// подсказка на весь слот — появляется даже если все фазы нулевые (таймаут).
		title := humanize.Time(ctx, p.T, time.UTC)
		for si, ms := range segments {
			title += " · " + latencySegmentNames[si] + " " + strconv.FormatUint(uint64(ms), 10) + "ms"
		}
		title += " · " + i18n.T(ctx, "chart.total") + " " + strconv.FormatUint(uint64(p.AvgTotalMs), 10) + "ms"
		if capped {
			title += " · " + i18n.T(ctx, "uptime.chart.over_scale")
		}
		writeHoverBand(&sb, g, slotX-gap/2, barW, title)
	}

	// та же столбчатая шкала времени, что у throughputBarsMarkup (точка i в g.x0+i*barW).
	bg := g
	bg.x1 = g.x0 + float64(n-1)*barW
	writeDeployMarker(&sb, bg, times, deploys)

	sb.WriteString(`</svg>`)
	return sb.String()
}

// цвета — те же токены, что у severityBadgeClass: trace/debug делят
// нейтральный, error/fatal — danger, как их бейджи.
var logSeverityClasses = map[string]string{
	log.SevTrace: "sev-trace",
	log.SevDebug: "sev-debug",
	log.SevInfo:  "sev-info",
	log.SevWarn:  "sev-warn",
	log.SevError: "sev-error",
	log.SevFatal: "sev-fatal",
}

// тот же каркас и приём укладки снизу вверх, что у latencyStackedSVG;
// times/series из ClickHouse — templ.Raw безопасен.
func logHistogramSVG(ctx context.Context, times []time.Time, series map[string][]int64, w, h int) templ.Component {
	return templ.Raw(logHistogramMarkup(ctx, times, series, w, h))
}

func logHistogramMarkup(ctx context.Context, times []time.Time, series map[string][]int64, w, h int) string {
	n := len(times)
	var maxSum int64
	for i := 0; i < n; i++ {
		var sum int64
		for _, sev := range log.Severities {
			sum += series[sev][i]
		}
		if sum > maxSum {
			maxSum = sum
		}
	}
	if n == 0 || maxSum == 0 {
		return chartEmptyAxis(w, h, i18n.T(ctx, "a11y.chart.logs_volume"))
	}

	g := newChartGeom(w, h, 48, 16, 26, 26)
	scale := newYScale(uint64(maxSum), 3)
	g = g.fitYLabels(scale, formatCountAxis)
	barW := g.barWidth(n)
	gap := barW * 0.15
	plotH := g.y1 - g.y0

	var sb strings.Builder
	// "latency-chart" переиспользован ради готовой разметки (кегль/font-size
	// для viewBox 720 уже есть в app.css), не по смыслу данных.
	sb.WriteString(svgRoot("latency-chart", w, h, i18n.T(ctx, "a11y.chart.logs_volume")))

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	writeYGrid(&sb, g, scale, formatCountAxis)
	writeXTicks(&sb, g, timeAxis(times, func(i int) float64 { return g.x0 + float64(i)*barW }, 70))
	sb.WriteString(`</g>`)

	// тот же приём зазора, что у стека задержек монитора.
	const segGap = 1.5

	for i := 0; i < n; i++ {
		slotX := g.x0 + float64(i)*barW + gap/2
		bw := barW - gap
		bottom := g.y1

		title := humanize.Time(ctx, times[i], time.UTC)
		for _, sev := range log.Severities {
			c := series[sev][i]
			if c == 0 {
				continue
			}
			segH := float64(c) / scale.top * plotH
			top := bottom - segH
			bottom = top
			drawY, drawH := top, segH
			if segH > segGap*2 {
				drawY, drawH = top+segGap, segH-segGap
			}
			sb.WriteString(`<rect x="`)
			sb.WriteString(formatCoord(slotX))
			sb.WriteString(`" y="`)
			sb.WriteString(formatCoord(drawY))
			sb.WriteString(`" width="`)
			sb.WriteString(formatCoord(bw))
			sb.WriteString(`" height="`)
			sb.WriteString(formatCoord(drawH))
			sb.WriteString(`" class="`)
			sb.WriteString(logSeverityClasses[sev])
			sb.WriteString(`"/>`)

			title += " · " + i18n.T(ctx, "logs.severity."+sev) + " " + strconv.FormatInt(c, 10)
		}

		// подсказка появляется и над пустой корзиной — просто без перечисления.
		writeHoverBand(&sb, g, slotX-gap/2, barW, title)
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}
