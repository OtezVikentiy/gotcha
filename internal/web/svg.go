package web

import (
	"context"
	"fmt"
	"hash/fnv"
	"html"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

const flameRowHeight = 18

// flamegraphSVG рисует icicle-диаграмму дерева профиля (сверху вниз). Ширина
// фрейма ∝ его доле от корня; глубина = уровень стека. Текст SVG строится из
// чисел и html-экранированных имён — templ.Raw безопасен. Пустое дерево
// (Value==0) → плейсхолдер «нет данных».
func flamegraphSVG(ctx context.Context, root *profile.FlameNode, width int) templ.Component {
	if root == nil || root.Value == 0 {
		return templ.Raw(`<p class="empty">` + html.EscapeString(i18n.T(ctx, "profile.flame.no_data")) + `</p>`)
	}
	depth := flameDepth(root)
	height := depth * flameRowHeight
	var sb strings.Builder
	sb.WriteString(`<svg class="flamegraph" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(width))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(height))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg" font-family="monospace" font-size="10">`)
	flameRow(&sb, root, 0, float64(width), 0, root.Value)
	sb.WriteString(`</svg>`)
	return templ.Raw(sb.String())
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

// flameRow рисует прямоугольник узла и рекурсивно детей. x/w — позиция и ширина
// в пикселях; total — Value корня (для доли в подписи).
func flameRow(sb *strings.Builder, n *profile.FlameNode, x, w float64, depth int, total uint64) {
	if w < 0.5 {
		return
	}
	y := depth * flameRowHeight
	pct := 0.0
	if total > 0 {
		pct = float64(n.Value) / float64(total) * 100
	}
	sb.WriteString(`<g><rect x="`)
	sb.WriteString(formatCoord(x))
	sb.WriteString(`" y="`)
	sb.WriteString(strconv.Itoa(y))
	sb.WriteString(`" width="`)
	sb.WriteString(formatCoord(w))
	sb.WriteString(`" height="`)
	sb.WriteString(strconv.Itoa(flameRowHeight - 1))
	sb.WriteString(`" fill="`)
	sb.WriteString(flameColor(n.Name))
	sb.WriteString(`"><title>`)
	sb.WriteString(html.EscapeString(n.Name))
	sb.WriteString(` — `)
	sb.WriteString(strconv.FormatFloat(pct, 'f', 1, 64))
	sb.WriteString(`%</title></rect>`)
	if w > 30 {
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(x + 2))
		sb.WriteString(`" y="`)
		sb.WriteString(strconv.Itoa(y + flameRowHeight - 6))
		sb.WriteString(`" fill="#111">`)
		sb.WriteString(html.EscapeString(truncateRunes(n.Name, int(w/6))))
		sb.WriteString(`</text>`)
	}
	sb.WriteString(`</g>`)
	childX := x
	for _, c := range n.Children {
		cw := w * float64(c.Value) / float64(n.Value)
		flameRow(sb, c, childX, cw, depth+1, total)
		childX += cw
	}
}

// flameColor — детерминированный тёплый цвет по имени функции.
func flameColor(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	hue := int(h.Sum32() % 40) // 0..40 — красно-оранжевый диапазон
	return fmt.Sprintf("hsl(%d,65%%,60%%)", hue+10)
}

// truncateRunes обрезает строку до n рун (без многоточия), n<=0 → пусто.
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

// metricThreshold — порог алерта для горизонтальной линии на графике метрики
// (значение + направление сравнения, чтобы подписать «> N» / «< N»).
type metricThreshold struct {
	Value      float64
	Comparator string // "gt" | "lt"
}

// metricSeriesSVG рисует график ряда metric.Point с осями: ось Y (значения +
// юнит слева), ось X (время снизу) и пунктирные пороговые линии алертов
// (Grafana-style). Текст SVG состоит из чисел и html-экранированных подписей —
// templ.Raw безопасен, как в latencyLinesSVG.
func metricSeriesSVG(ctx context.Context, points []metric.Point, unit string, thresholds []metricThreshold, w, h int) templ.Component {
	return templ.Raw(metricSeriesMarkup(ctx, points, unit, thresholds, w, h))
}

func metricSeriesMarkup(ctx context.Context, points []metric.Point, unit string, thresholds []metricThreshold, w, h int) string {
	const (
		padL = 58 // место под подписи оси Y
		padR = 16
		padT = 12
		padB = 26 // место под подписи оси X
	)
	x0, x1 := float64(padL), float64(w-padR)
	y0, y1 := float64(padT), float64(h-padB)

	var sb strings.Builder
	sb.WriteString(`<svg class="metric-chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)

	// Рамка осей (левая вертикаль + нижняя горизонталь).
	sb.WriteString(`<g class="chart-axis">`)
	axisLine(&sb, x0, y0, x0, y1)
	axisLine(&sb, x0, y1, x1, y1)

	if len(points) == 0 {
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord((x0 + x1) / 2))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord((y0 + y1) / 2))
		sb.WriteString(`" text-anchor="middle" dominant-baseline="middle" fill="currentColor">`)
		sb.WriteString(html.EscapeString(i18n.T(ctx, "chart.no_data_period")))
		sb.WriteString(`</text></g></svg>`)
		return sb.String()
	}

	// Домен значений: данные + пороги (чтобы пороговые линии попадали в область).
	dataMin, dataMax := points[0].V, points[0].V
	for _, p := range points {
		if p.V < dataMin {
			dataMin = p.V
		}
		if p.V > dataMax {
			dataMax = p.V
		}
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

	// Подписи оси Y: max, середина, min значений данных + горизонтальные линии.
	for _, v := range []float64{dataMax, (dataMin + dataMax) / 2, dataMin} {
		yv := yFor(v)
		axisLine(&sb, x0, yv, x1, yv)
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(x0 - 6))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(yv))
		sb.WriteString(`" text-anchor="end" dominant-baseline="middle" fill="currentColor">`)
		sb.WriteString(html.EscapeString(formatAxisValue(v, unit)))
		sb.WriteString(`</text>`)
	}

	// Подписи оси X: время первой, средней и последней точки.
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

	// Пороговые линии алертов (пунктир, поверх сетки, под линией данных).
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

	// Линия данных.
	var pts strings.Builder
	for i, p := range points {
		x := x0
		if n > 1 {
			x = x0 + float64(i)/float64(n-1)*(x1-x0)
		}
		if i > 0 {
			pts.WriteByte(' ')
		}
		pts.WriteString(formatCoord(x))
		pts.WriteByte(',')
		pts.WriteString(formatCoord(yFor(p.V)))
	}
	sb.WriteString(`<polyline points="`)
	sb.WriteString(pts.String())
	sb.WriteString(`" fill="none" stroke="#3d7bff" stroke-width="1.5"/>`)
	sb.WriteString(`</svg>`)
	return sb.String()
}

// axisLine — тонкая линия сетки/оси в текущем цвете (currentColor группы).
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

// comparatorSymbol — знак сравнения для подписи пороговой линии.
func comparatorSymbol(cmp string) string {
	if cmp == "lt" {
		return "<"
	}
	return ">"
}

// formatAxisValue форматирует значение для подписи оси: до 3 значащих цифр, с
// суффиксом k/M для крупных чисел и опциональным юнитом.
func formatAxisValue(v float64, unit string) string {
	abs := v
	if abs < 0 {
		abs = -abs
	}
	var s string
	switch {
	case abs >= 1e6:
		s = strconv.FormatFloat(v/1e6, 'g', 3, 64) + "M"
	case abs >= 1e3:
		s = strconv.FormatFloat(v/1e3, 'g', 3, 64) + "k"
	default:
		s = strconv.FormatFloat(v, 'g', 3, 64)
	}
	// "1" — юнит безразмерной метрики по соглашению OTLP (счётчики,
	// количества). На оси его печатать нельзя: «17 1» читается как одно
	// число, а не как «17 штук».
	if unit != "" && unit != "1" {
		s += " " + unit
	}
	return s
}

// metricTimeLabel форматирует момент времени для оси X: на окне до двух суток —
// часы:минуты, на более длинном — день.месяц.
func metricTimeLabel(t time.Time, spanHours float64) string {
	if spanHours >= 48 {
		return t.Format("02.01")
	}
	return t.Format("15:04")
}

// sparklineWidth/Height — размер инлайновых SVG-спарклайнов в списке issues.
const (
	sparklineWidth  = 96
	sparklineHeight = 24
)

// sparklineSVG строит инлайновый SVG-спарклайн: полилиния по значениям
// buckets, нормированным на максимум. Пустые данные (buckets==nil/пустой
// слайс, либо все нули) рисуются плоской линией посередине, чтобы не путать
// "нет данных" с ошибкой рендера.
//
// buckets приходят из event.Query.Sparklines (числа, посчитанные CH), поэтому
// собранный из них SVG-текст не требует HTML-экранирования — templ.Raw здесь
// безопасен, так как в него не попадает ничего, кроме чисел, отформатированных
// этой функцией.
func sparklineSVG(buckets []uint64, w, h int) templ.Component {
	return templ.Raw(sparklinePolyline(buckets, w, h))
}

func sparklinePolyline(buckets []uint64, w, h int) string {
	var max uint64
	for _, v := range buckets {
		if v > max {
			max = v
		}
	}
	if len(buckets) == 0 || max == 0 {
		return flatlineSVG(w, h)
	}

	n := len(buckets)
	var points strings.Builder
	for i, v := range buckets {
		var x float64
		if n > 1 {
			x = float64(i) / float64(n-1) * float64(w)
		}
		y := float64(h) - float64(v)/float64(max)*float64(h)
		if i > 0 {
			points.WriteByte(' ')
		}
		points.WriteString(formatCoord(x))
		points.WriteByte(',')
		points.WriteString(formatCoord(y))
	}

	var sb strings.Builder
	sb.WriteString(`<svg class="sparkline" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg"><polyline points="`)
	sb.WriteString(points.String())
	sb.WriteString(`" fill="none" stroke="currentColor" stroke-width="1.5"/></svg>`)
	return sb.String()
}

// flatlineSVG — горизонтальная линия посередине: issue без событий в окне
// спарклайна (или без данных вовсе).
func flatlineSVG(w, h int) string {
	y := formatCoord(float64(h) / 2)
	var sb strings.Builder
	sb.WriteString(`<svg class="sparkline" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg"><polyline points="0,`)
	sb.WriteString(y)
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(',')
	sb.WriteString(y)
	sb.WriteString(`" fill="none" stroke="currentColor" stroke-width="1.5"/></svg>`)
	return sb.String()
}

func formatCoord(f float64) string {
	// Защита: нефинитное значение (NaN/±Inf) дало бы SVG-атрибут "NaN"/"+Inf" и
	// сломало бы отрисовку. Пороги NaN/Inf уже отсекаются на входе, но значение
	// ряда из ClickHouse теоретически может прийти нефинитным — клампим в 0.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "0.0"
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}

// perfSparklineWidth/Height — размер инлайнового спарклайна p95 в списке
// эндпойнтов (та же роль, что sparkline у issues).
const (
	perfSparklineWidth  = 96
	perfSparklineHeight = 24
)

// latencySparklineSVG строит спарклайн p95 по ряду trace.LatencyPoint —
// переиспользует sparklineSVG, скармливая ему P95 каждой точки как []uint64.
// Числа приходят из trace.Query.EndpointLatency (посчитаны CH), поэтому
// templ.Raw внутри sparklineSVG остаётся безопасным.
func latencySparklineSVG(points []trace.LatencyPoint, w, h int) templ.Component {
	vals := make([]uint64, len(points))
	for i, p := range points {
		vals[i] = uint64(p.P95)
	}
	return sparklineSVG(vals, w, h)
}

// perfLatencyChartWidth/Height — размер графика перцентилей p50/p95 и графика
// throughput на странице эндпойнта.
const (
	perfLatencyChartWidth  = 1200
	perfLatencyChartHeight = 220
)

// perfLatencyLineClasses — классы линий p50 и p95 на графике перцентилей;
// цвет назначается в app.css из токенов, чтобы линии следовали теме.
// Захардкожены (не currentColor): нужны два разных цвета в одном SVG.
var perfLatencyLineClasses = [2]string{"series-p50", "series-p95"}

// latencyLinesSVG рисует две полилинии (p50 и p95) по ряду trace.LatencyPoint,
// нормированные на максимум p95. Пустой ряд (или все нули) → плоская линия
// посередине, тем же принципом «нет данных ≠ ошибка рендера», что и
// flatlineSVG.
//
// points приходят из trace.Query.EndpointLatency (числа), поэтому собранный
// SVG-текст состоит только из чисел и фиксированных цветов — templ.Raw
// безопасен по тем же причинам, что и в sparklineSVG.
func latencyLinesSVG(ctx context.Context, points []trace.LatencyPoint, w, h int) templ.Component {
	return templ.Raw(latencyLinesMarkup(ctx, points, w, h))
}

// latencyLinesMarkup — перцентили p50/p95 во времени с осями, сеткой и
// подсказками. Раньше это была голая ломаная без единой подписи: ни величины
// (микросекунды? миллисекунды?), ни времени, ни какая линия что означает.
func latencyLinesMarkup(ctx context.Context, points []trace.LatencyPoint, w, h int) string {
	var max uint32
	for _, p := range points {
		if p.P95 > max {
			max = p.P95
		}
	}
	if len(points) == 0 || max == 0 {
		return flatlineSVG(w, h)
	}

	g := newChartGeom(w, h, 64, 16, 26, 26)
	scale := newYScaleFloat(float64(max), 3)
	n := len(points)

	var sb strings.Builder
	sb.WriteString(`<svg class="latency-chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	writeYGrid(&sb, g, scale, formatUSAxis)
	times := make([]time.Time, n)
	for i, p := range points {
		times[i] = p.T
	}
	writeXTicks(&sb, g, timeAxis(times, func(i int) float64 { return g.xForIndex(i, n) }, 70))
	sb.WriteString(`</g>`)

	// Линии перцентилей.
	series := [2]func(trace.LatencyPoint) uint32{
		func(p trace.LatencyPoint) uint32 { return p.P50 },
		func(p trace.LatencyPoint) uint32 { return p.P95 },
	}
	for si, pick := range series {
		var pts strings.Builder
		for i, p := range points {
			if i > 0 {
				pts.WriteByte(' ')
			}
			pts.WriteString(formatCoord(g.xForIndex(i, n)))
			pts.WriteByte(',')
			pts.WriteString(formatCoord(scale.yFor(g, float64(pick(p)))))
		}
		sb.WriteString(`<polyline points="`)
		sb.WriteString(pts.String())
		sb.WriteString(`" fill="none" class="`)
		sb.WriteString(perfLatencyLineClasses[si])
		sb.WriteString(`" stroke-width="1.5"/>`)
	}

	// Полосы наведения: по одной на точку, с обоими перцентилями в подсказке.
	band := (g.x1 - g.x0) / float64(n)
	for i, p := range points {
		writeHoverBand(&sb, g, g.xForIndex(i, n)-band/2, band,
			p.T.UTC().Format("02.01 15:04")+" · p50 "+formatUSAxis(float64(p.P50))+
				" · p95 "+formatUSAxis(float64(p.P95))+" · "+
				i18n.Tn(ctx, "chart.bar.transactions", int(p.Count)))
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}

func throughputBarsSVG(ctx context.Context, points []trace.LatencyPoint, w, h int) templ.Component {
	return templ.Raw(throughputBarsMarkup(ctx, points, w, h))
}

// throughputBarsMarkup — число транзакций за интервал агрегации, столбиками,
// с осями и подсказкой на каждом столбике.
func throughputBarsMarkup(ctx context.Context, points []trace.LatencyPoint, w, h int) string {
	var max uint64
	for _, p := range points {
		if p.Count > max {
			max = p.Count
		}
	}
	if len(points) == 0 || max == 0 {
		return chartEmptyAxis(w, h)
	}

	g := newChartGeom(w, h, 48, 16, 26, 26)
	scale := newYScale(max, 3)
	n := len(points)
	barW := g.barWidth(n)
	gap := barW * 0.15

	var sb strings.Builder
	sb.WriteString(`<svg class="chart-freq" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	writeYGrid(&sb, g, scale, func(v float64) string {
		return strconv.FormatFloat(v, 'f', 0, 64)
	})
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
		sb.WriteString(html.EscapeString(p.T.UTC().Format("02.01 15:04") + " — " +
			i18n.Tn(ctx, "chart.bar.transactions", int(p.Count))))
		sb.WriteString(`</title></rect>`)
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}

func durationHistogramSVG(ctx context.Context, buckets []trace.DurationBucket, w, h int) templ.Component {
	return templ.Raw(durationHistogramMarkup(ctx, buckets, w, h))
}

// durationHistogramMarkup — распределение длительностей: по X границы корзин
// в миллисекундах, по Y число транзакций. Без подписей осей величина не
// угадывалась вообще: столбики могли означать что угодно.
func durationHistogramMarkup(ctx context.Context, buckets []trace.DurationBucket, w, h int) string {
	var max uint64
	for _, b := range buckets {
		if b.Count > max {
			max = b.Count
		}
	}
	if len(buckets) == 0 || max == 0 {
		return chartEmptyAxis(w, h)
	}

	g := newChartGeom(w, h, 48, 16, 26, 26)
	scale := newYScale(max, 3)
	n := len(buckets)
	barW := g.barWidth(n)
	gap := barW * 0.15

	var sb strings.Builder
	sb.WriteString(`<svg class="chart-freq" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)

	sb.WriteString(`<g class="chart-axis">`)
	writeFrame(&sb, g)
	writeYGrid(&sb, g, scale, func(v float64) string {
		return strconv.FormatFloat(v, 'f', 0, 64)
	})
	// Подписи по X — верхние границы корзин, но не каждая: их до двадцати, и
	// подписи наезжали бы друг на друга.
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

// chartWidth/Height — размер bar-chart частоты на странице issue (события за 7
// дней). Высота с запасом под подписи оси X, ширина под подписи оси Y.
const (
	chartWidth  = 1200
	chartHeight = 180
)

// chartPad* — поля графика частоты под оси.
const (
	chartPadL = 40
	chartPadR = 10
	chartPadT = 10
	chartPadB = 22
)

// chartSVG строит инлайновый SVG bar-chart: один столбик на точку
// event.Point, высота нормирована на максимум N в points. Пустые данные
// (points==nil или все N==0) рисуют плоскую ось у нижнего края, тем же
// принципом, что flatlineSVG у sparklineSVG — "нет событий" не должно
// выглядеть как ошибка рендера.
//
// points приходят из event.Query.Series (числа, посчитанные CH), поэтому
// собранный SVG-текст состоит только из чисел, отформатированных этой
// функцией — templ.Raw здесь безопасен по тем же причинам, что и в
// sparklineSVG.
func chartSVG(ctx context.Context, points []event.Point, w, h int) templ.Component {
	return templ.Raw(chartBars(ctx, points, w, h))
}

// niceStep подбирает «круглый» шаг сетки из ряда 1/2/5×10ⁿ так, чтобы линий
// вышло примерно targetLines. Без него подписи оси получаются вида 37/74/111 —
// формально верные, но прикинуть по ним значение столбика нельзя.
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

// niceStepFloat — тот же ряд 1/2/5×10ⁿ, но для дробных величин (длительности
// в микросекундах), где округление шага до целого бессмысленно.
func niceStepFloat(max float64, targetLines int) float64 {
	if max <= 0 || targetLines <= 0 {
		return 1
	}
	raw := max / float64(targetLines)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
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

	var sb strings.Builder
	// Пропорции сохраняем (preserveAspectRatio по умолчанию): у графика есть
	// текстовые подписи осей, и неравномерное растяжение растягивало бы вместе
	// с рисунком и буквы. Чтобы график занимал широкую карточку целиком,
	// увеличены сами размеры холста (chartWidth/chartHeight ниже), а не
	// способ его вписывания.
	sb.WriteString(`<svg class="chart-freq" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)

	// Оси: левая вертикаль + базовая линия.
	sb.WriteString(`<g class="chart-axis">`)
	axisLine(&sb, x0, y0, x0, y1)
	axisLine(&sb, x0, y1, x1, y1)

	var max uint64
	for _, p := range points {
		if p.N > max {
			max = p.N
		}
	}
	if len(points) == 0 || max == 0 {
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(x0 - 6))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(y1))
		sb.WriteString(`" text-anchor="end" dominant-baseline="middle" fill="currentColor">0</text></g></svg>`)
		return sb.String()
	}

	// Горизонтальная сетка: круглый шаг, подпись на каждой линии. Верх шкалы
	// берём строго выше максимума — на один шаг над ближайшим кратным. Если
	// верх совпадает с максимумом, самый высокий столбик упирается в рамку и
	// график читается как сплошной забор; небольшой запас сверху задаёт
	// «шапку», по которой видно, что пик — это пик.
	step := niceStep(max, 3)
	top := (max/step + 1) * step
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

	// Вертикальная сетка: линия и подпись на каждой границе суток. Шаг
	// корзины меньше суток (3 часа), поэтому подписывать каждую корзину
	// нечитаемо — привязку ко времени даёт день.
	n := len(points)
	barW := (x1 - x0) / float64(n)
	for i, p := range points {
		if i > 0 && p.T.UTC().YearDay() == points[i-1].T.UTC().YearDay() {
			continue
		}
		x := x0 + float64(i)*barW
		if i > 0 {
			axisLine(&sb, x, y0, x, y1)
		}
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(x))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(float64(h) - 7))
		sb.WriteString(`" text-anchor="middle" fill="currentColor">`)
		sb.WriteString(html.EscapeString(p.T.UTC().Format("02.01")))
		sb.WriteString(`</text>`)
	}
	sb.WriteString(`</g>`)

	// Столбики в области графика. У каждого — <title> с временем корзины и
	// количеством: это нативная подсказка браузера при наведении, она не
	// требует JS и переживает его отключение.
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
		sb.WriteString(html.EscapeString(p.T.UTC().Format("02.01 15:04")))
		sb.WriteString(` — `)
		sb.WriteString(html.EscapeString(i18n.Tn(ctx, "chart.bar.events", int(p.N))))
		sb.WriteString(`</title></rect>`)
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}

// chartEmptyAxis — горизонтальная линия у нижнего края: пустой ряд (нет данных)
// у bar-графиков с классом .chart (throughput, гистограмма длительностей) —
// "нет данных" не должно выглядеть как ошибка рендера.
func chartEmptyAxis(w, h int) string {
	y := formatCoord(float64(h) - 0.5)
	var sb strings.Builder
	sb.WriteString(`<svg class="chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg" preserveAspectRatio="none"><line x1="0" y1="`)
	sb.WriteString(y)
	sb.WriteString(`" x2="`)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteString(`" y2="`)
	sb.WriteString(y)
	sb.WriteString(`" stroke="currentColor" stroke-width="1"/></svg>`)
	return sb.String()
}

// availabilityBarsWidth/Height — размер полоски доступности в списке
// мониторов и на странице монитора (план 4, задача 2): по умолчанию 24
// корзины (например, часовые за последние 24 часа).
const (
	availabilityBarsWidth  = 192
	availabilityBarsHeight = 24
)

// Классы корзин полоски доступности: зелёная (все проверки в корзине
// успешны), красная (хотя бы одна провалилась), серая (в корзине нет ни одной
// проверки — "нет данных", не путать с провалом). Цвет назначает app.css из
// токенов, поэтому полоска следует теме. Одного currentColor тут мало —
// нужны три разных цвета в одном SVG, а не один цвет из контекста, как у
// sparklineSVG/chartSVG.
const (
	availabilityClassUp    = "bar-up"
	availabilityClassDown  = "bar-down"
	availabilityClassEmpty = "bar-empty"
)

// availabilityBarsSVG строит полоску доступности: один прямоугольник на
// корзину uptime.Query.Bars. Пустой bars (buckets==nil/пустой слайс) рисует
// один серый прямоугольник на всю ширину — тот же принцип "нет данных не
// должно выглядеть как ошибка рендера", что и у flatlineSVG/chartEmptyAxis.
//
// bars приходят из uptime.Query.Bars (числа), поэтому собранный SVG-текст
// состоит только из чисел и трёх фиксированных цветовых констант выше —
// templ.Raw здесь безопасен по тем же причинам, что и в sparklineSVG.
func availabilityBarsSVG(ctx context.Context, bars []uptime.UptimeStat, w, h int) templ.Component {
	return templ.Raw(availabilityBarsMarkup(ctx, bars, w, h))
}

func availabilityBarsMarkup(ctx context.Context, bars []uptime.UptimeStat, w, h int) string {
	if len(bars) == 0 {
		return availabilityEmptyBarsSVG(w, h)
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
	sb.WriteString(`<svg class="availability-bars" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)
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
	default:
		return availabilityClassDown
	}
}

// availabilityBarLabel — текстовая альтернатива цвету корзины полоски
// доступности (для <title> внутри <rect>): цвет — единственный сигнал
// состояния в SVG, без title screen reader / hover ничего не получают.
// uptime.UptimeStat не несёт даты/лейбла корзины, поэтому подпись — только
// состояние. Текст приходит из каталога, поэтому на вызывающей стороне он
// html-экранируется (контракт templ.Raw требует экранировать всё, что не
// является числом или фиксированной строкой самого шаблона).
func availabilityBarLabel(ctx context.Context, b uptime.UptimeStat) string {
	switch {
	case b.Total == 0:
		return i18n.T(ctx, "chart.no_data")
	case b.OK == b.Total:
		return i18n.T(ctx, "chart.bar.up")
	default:
		return i18n.T(ctx, "chart.bar.down")
	}
}

func availabilityEmptyBarsSVG(w, h int) string {
	var sb strings.Builder
	sb.WriteString(`<svg class="availability-bars" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg"><rect x="0" y="0" width="`)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteString(`" height="`)
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" class="`)
	sb.WriteString(availabilityClassEmpty)
	sb.WriteString(`"/></svg>`)
	return sb.String()
}

// waterfall* — геометрия SVG-waterfall трейса (этап 3, план 4, задача 3): по
// строке на спан, слева колонка подписей (op + мс) с отступом по глубине
// дерева, справа полоса, спозиционированная по времени спана в масштабе всего
// трейса. waterfallMaxRows — потолок отрисованных строк: трейс из тысяч спанов
// не должен родить чудовищный SVG, поэтому рисуем первые N в порядке обхода
// дерева, а страница сообщает, что показаны не все (см. trace.templ).
const (
	waterfallWidth   = 900
	waterfallRowH    = 18
	waterfallLabelW  = 300
	waterfallPadX    = 4
	waterfallIndent  = 12
	waterfallMaxRows = 200
)

// waterfallClassOK/Error — класс полосы спана: обычный (status == ok и нет
// привязанной ошибки) и ошибочный (status != ok либо на спане есть событие-
// ошибка). Цвет назначает app.css из токенов, как у availabilityClass* —
// нужны два разных цвета в одном SVG, одного currentColor мало.
const (
	waterfallClassOK    = "wf-ok"
	waterfallClassError = "wf-err"
)

// waterfallSVG строит SVG-waterfall трейса: дерево спанов (по ParentSpanID)
// разворачивается в порядке обхода в глубину, каждая строка — полоса,
// спозиционированная по StartUS..StartUS+DurationUS в масштабе totalUS, с
// отступом подписи по глубине. Спаны из errIssues (span_id → issue_id)
// красятся красным и оборачиваются ссылкой на /issues/{issue_id}. Число строк
// ограничено waterfallMaxRows. Пустой трейс не рисуется (nil-компонент через
// пустую строку не отдаём — вызывающая сторона не зовёт нас на пустом трейсе).
//
// op/description спанов — недоверенные данные, поэтому подписи экранируются
// (templ.EscapeString): в отличие от прочих SVG-хелперов здесь в текст SVG
// попадают строки пользователя, а не только числа, поэтому templ.Raw без
// экранирования был бы XSS-дырой.
func waterfallSVG(spans []trace.SpanRow, errIssues map[string]int64, totalUS uint32, w int) templ.Component {
	return templ.Raw(waterfallMarkup(spans, errIssues, totalUS, w))
}

func waterfallMarkup(spans []trace.SpanRow, errIssues map[string]int64, totalUS uint32, w int) string {
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
	b.WriteString(`<svg class="waterfall" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(w))
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(h))
	b.WriteString(`" xmlns="http://www.w3.org/2000/svg" font-family="monospace" font-size="10">`)

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
		b.WriteString(`"/>`)

		labelX := waterfallPadX + os.depth*waterfallIndent
		b.WriteString(`<text x="`)
		b.WriteString(strconv.Itoa(labelX))
		b.WriteString(`" y="`)
		b.WriteString(formatCoord(y + float64(waterfallRowH) - 5))
		b.WriteString(`" class="waterfall-label">`)
		b.WriteString(templ.EscapeString(waterfallLabel(s)))
		b.WriteString(`</text>`)

		if isErr {
			b.WriteString(`</a>`)
		}
	}

	b.WriteString(`</svg>`)
	return b.String()
}

// orderedSpan — спан в порядке обхода дерева с его глубиной.
type orderedSpan struct {
	span  trace.SpanRow
	depth int
}

// orderSpanTree разворачивает спаны в порядок обхода в глубину: корни (спаны
// без родителя или с родителем вне трейса) в исходном порядке (спаны приходят
// отсортированными по времени), под каждым — его дети рекурсивно. Возвращает
// не более max строк. Циклы (спан ссылается на предка) обрезаются посещением.
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

// waterfallLabel — подпись строки: op и длительность в мс. op недоверенный,
// экранируется вызывающей стороной.
func waterfallLabel(s trace.SpanRow) string {
	op := s.Op
	if op == "" {
		op = s.Description
	}
	return op + " " + waterfallMS(s.DurationUS)
}

// waterfallMS форматирует микросекунды человекочитаемо (µs→ms→s), как
// formatDurationUS в templates, но локально — svg.go в другом пакете.
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

// perfVitalChartWidth/Height — размер мини-графика p75 web vital во времени на
// панели Web Vitals страницы эндпойнта (этап 4, план 2, задача 2).
const (
	perfVitalChartWidth  = 240
	perfVitalChartHeight = 48
)

// vitalSeriesSVG рисует полилинию p75 одного web vital по ряду
// trace.VitalPoint, нормированную на максимум P75. Пустой ряд (или все нули) →
// плоская линия посередине, тем же принципом «нет данных ≠ ошибка рендера»,
// что и flatlineSVG.
//
// points приходят из trace.Query.VitalSeries (числа, посчитанные CH), поэтому
// собранный SVG-текст состоит только из чисел — templ.Raw безопасен по тем же
// причинам, что и в sparklineSVG.
// vitalSeriesSVG — врезка-спарклайн Web Vital. Осей ей не даём: это график
// шириной в пару сантиметров внутри строки таблицы, оси его только
// загромоздят. Вместо этого — подсказка с диапазоном и последним значением;
// format приводит число к той же записи, что и значение рядом в строке
// (миллисекунды/секунды либо безразмерный CLS), иначе в подсказке висели бы
// голые числа без единицы.
func vitalSeriesSVG(points []trace.VitalPoint, w, h int, format func(float64) string) templ.Component {
	return templ.Raw(vitalSeriesMarkup(points, w, h, format))
}

func vitalSeriesMarkup(points []trace.VitalPoint, w, h int, format func(float64) string) string {
	var max float64
	for _, p := range points {
		if p.P75 > max {
			max = p.P75
		}
	}
	if len(points) == 0 || max <= 0 {
		return flatlineSVG(w, h)
	}

	n := len(points)
	var pts strings.Builder
	for i, p := range points {
		var x float64
		if n > 1 {
			x = float64(i) / float64(n-1) * float64(w)
		}
		y := float64(h) - p.P75/max*float64(h)
		if i > 0 {
			pts.WriteByte(' ')
		}
		pts.WriteString(formatCoord(x))
		pts.WriteByte(',')
		pts.WriteString(formatCoord(y))
	}

	var sb strings.Builder
	sb.WriteString(`<svg class="vital-chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)
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
			points[0].T.UTC().Format("02.01") + " – " + last.T.UTC().Format("02.01") +
				" · min " + format(lo) + " · max " + format(hi) + " · " + format(last.P75)))
		sb.WriteString(`</title>`)
	}
	sb.WriteString(`<polyline points="`)
	sb.WriteString(pts.String())
	sb.WriteString(`" fill="none" stroke="currentColor" stroke-width="1.5"/></svg>`)
	return sb.String()
}

// latencyChartWidth/Height — размер stacked-bar-графика задержек на странице
// монитора.
const (
	latencyChartWidth  = 480
	latencyChartHeight = 120
)

// latencySegmentClasses — классы сегментов stacked-bar-графика задержек, по
// порядку укладки снизу вверх: DNS, connect, TLS, TTFB. Цвет назначает
// app.css из токенов по той же причине, что и availabilityClass* выше —
// четыре разных цвета в одном SVG, одного currentColor мало.
var latencySegmentClasses = [4]string{"seg-dns", "seg-connect", "seg-tls", "seg-ttfb"}

// latencyStackedSVG строит один stacked-bar-график по сегментам таймингов
// (DNS/connect/TLS/TTFB) на точку временного ряда uptime.Query.Latency.
// Высота нормирована на максимум AvgTotalMs среди points; сумма
// DNS+connect+TLS+TTFB обычно меньше total_ms (остаток — тело ответа и
// прочий оверхед вне этой разбивки), поэтому столбики сегментов не всегда
// доходят до верхней границы бакета total — это ожидаемо, график показывает
// только известную разбивку, а не весь total.
//
// points приходят из uptime.Query.Latency (числа), поэтому собранный
// SVG-текст состоит только из чисел и фиксированных цветов —
// templ.Raw здесь безопасен по тем же причинам, что и в sparklineSVG.
func latencyStackedSVG(points []uptime.LatencyPoint, w, h int) templ.Component {
	return templ.Raw(latencyStackedMarkup(points, w, h))
}

func latencyStackedMarkup(points []uptime.LatencyPoint, w, h int) string {
	var max uint32
	for _, p := range points {
		if p.AvgTotalMs > max {
			max = p.AvgTotalMs
		}
	}
	if len(points) == 0 || max == 0 {
		return chartEmptyAxis(w, h)
	}

	n := len(points)
	barW := float64(w) / float64(n)
	gap := barW * 0.15

	var bars strings.Builder
	for i, p := range points {
		x := float64(i)*barW + gap/2
		segments := [4]uint32{p.AvgDNSMs, p.AvgConnectMs, p.AvgTLSMs, p.AvgTTFBMs}
		y := float64(h)
		for si, ms := range segments {
			segH := float64(ms) / float64(max) * float64(h)
			y -= segH
			bars.WriteString(`<rect x="`)
			bars.WriteString(formatCoord(x))
			bars.WriteString(`" y="`)
			bars.WriteString(formatCoord(y))
			bars.WriteString(`" width="`)
			bars.WriteString(formatCoord(barW - gap))
			bars.WriteString(`" height="`)
			bars.WriteString(formatCoord(segH))
			bars.WriteString(`" class="`)
			bars.WriteString(latencySegmentClasses[si])
			bars.WriteString(`"/>`)
		}
	}

	var sb strings.Builder
	sb.WriteString(`<svg class="latency-chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg" preserveAspectRatio="none">`)
	sb.WriteString(bars.String())
	sb.WriteString(`</svg>`)
	return sb.String()
}
