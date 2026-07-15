package web

import (
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

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
	perfLatencyChartWidth  = 480
	perfLatencyChartHeight = 120
)

// perfLatencyLineColors — цвета линий p50 и p95 на графике перцентилей.
// Захардкожены (не currentColor): нужны два разных цвета в одном SVG.
var perfLatencyLineColors = [2]string{"#5b8cff", "#e0a52c"}

// latencyLinesSVG рисует две полилинии (p50 и p95) по ряду trace.LatencyPoint,
// нормированные на максимум p95. Пустой ряд (или все нули) → плоская линия
// посередине, тем же принципом «нет данных ≠ ошибка рендера», что и
// flatlineSVG.
//
// points приходят из trace.Query.EndpointLatency (числа), поэтому собранный
// SVG-текст состоит только из чисел и фиксированных цветов — templ.Raw
// безопасен по тем же причинам, что и в sparklineSVG.
func latencyLinesSVG(points []trace.LatencyPoint, w, h int) templ.Component {
	return templ.Raw(latencyLinesMarkup(points, w, h))
}

func latencyLinesMarkup(points []trace.LatencyPoint, w, h int) string {
	var max uint32
	for _, p := range points {
		if p.P95 > max {
			max = p.P95
		}
	}
	if len(points) == 0 || max == 0 {
		return flatlineSVG(w, h)
	}

	n := len(points)
	series := [2]func(trace.LatencyPoint) uint32{
		func(p trace.LatencyPoint) uint32 { return p.P50 },
		func(p trace.LatencyPoint) uint32 { return p.P95 },
	}

	var lines strings.Builder
	for si, pick := range series {
		var pts strings.Builder
		for i, p := range points {
			var x float64
			if n > 1 {
				x = float64(i) / float64(n-1) * float64(w)
			}
			y := float64(h) - float64(pick(p))/float64(max)*float64(h)
			if i > 0 {
				pts.WriteByte(' ')
			}
			pts.WriteString(formatCoord(x))
			pts.WriteByte(',')
			pts.WriteString(formatCoord(y))
		}
		lines.WriteString(`<polyline points="`)
		lines.WriteString(pts.String())
		lines.WriteString(`" fill="none" stroke="`)
		lines.WriteString(perfLatencyLineColors[si])
		lines.WriteString(`" stroke-width="1.5"/>`)
	}

	var sb strings.Builder
	sb.WriteString(`<svg class="latency-chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)
	sb.WriteString(lines.String())
	sb.WriteString(`</svg>`)
	return sb.String()
}

// throughputBarsSVG рисует bar-chart числа транзакций по времени (Count каждой
// точки ряда). Пустой ряд → плоская ось у нижнего края (chartEmptyAxis).
//
// points приходят из trace.Query.EndpointLatency (числа) — templ.Raw безопасен.
func throughputBarsSVG(points []trace.LatencyPoint, w, h int) templ.Component {
	return templ.Raw(throughputBarsMarkup(points, w, h))
}

func throughputBarsMarkup(points []trace.LatencyPoint, w, h int) string {
	var max uint64
	for _, p := range points {
		if p.Count > max {
			max = p.Count
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
		barH := float64(p.Count) / float64(max) * float64(h)
		x := float64(i)*barW + gap/2
		y := float64(h) - barH
		bars.WriteString(`<rect x="`)
		bars.WriteString(formatCoord(x))
		bars.WriteString(`" y="`)
		bars.WriteString(formatCoord(y))
		bars.WriteString(`" width="`)
		bars.WriteString(formatCoord(barW - gap))
		bars.WriteString(`" height="`)
		bars.WriteString(formatCoord(barH))
		bars.WriteString(`" fill="currentColor"/>`)
	}

	var sb strings.Builder
	sb.WriteString(`<svg class="chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)
	sb.WriteString(bars.String())
	sb.WriteString(`</svg>`)
	return sb.String()
}

// durationHistogramSVG рисует гистограмму длительностей: столбик на корзину
// trace.DurationBucket, высота нормирована на максимальный Count. Пустая
// гистограмма → плоская ось у нижнего края (chartEmptyAxis).
//
// buckets приходят из trace.Query.DurationHistogram (числа) — templ.Raw
// безопасен.
func durationHistogramSVG(buckets []trace.DurationBucket, w, h int) templ.Component {
	return templ.Raw(durationHistogramMarkup(buckets, w, h))
}

func durationHistogramMarkup(buckets []trace.DurationBucket, w, h int) string {
	var max uint64
	for _, b := range buckets {
		if b.Count > max {
			max = b.Count
		}
	}
	if len(buckets) == 0 || max == 0 {
		return chartEmptyAxis(w, h)
	}

	n := len(buckets)
	barW := float64(w) / float64(n)
	gap := barW * 0.15

	var bars strings.Builder
	for i, b := range buckets {
		barH := float64(b.Count) / float64(max) * float64(h)
		x := float64(i)*barW + gap/2
		y := float64(h) - barH
		bars.WriteString(`<rect x="`)
		bars.WriteString(formatCoord(x))
		bars.WriteString(`" y="`)
		bars.WriteString(formatCoord(y))
		bars.WriteString(`" width="`)
		bars.WriteString(formatCoord(barW - gap))
		bars.WriteString(`" height="`)
		bars.WriteString(formatCoord(barH))
		bars.WriteString(`" fill="currentColor"/>`)
	}

	var sb strings.Builder
	sb.WriteString(`<svg class="chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)
	sb.WriteString(bars.String())
	sb.WriteString(`</svg>`)
	return sb.String()
}

// chartWidth/Height — размер инлайнового bar-chart на странице issue
// (частота событий за 7 дней).
const (
	chartWidth  = 320
	chartHeight = 96
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
func chartSVG(points []event.Point, w, h int) templ.Component {
	return templ.Raw(chartBars(points, w, h))
}

func chartBars(points []event.Point, w, h int) string {
	var max uint64
	for _, p := range points {
		if p.N > max {
			max = p.N
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
		barH := float64(p.N) / float64(max) * float64(h)
		x := float64(i)*barW + gap/2
		y := float64(h) - barH
		bars.WriteString(`<rect x="`)
		bars.WriteString(formatCoord(x))
		bars.WriteString(`" y="`)
		bars.WriteString(formatCoord(y))
		bars.WriteString(`" width="`)
		bars.WriteString(formatCoord(barW - gap))
		bars.WriteString(`" height="`)
		bars.WriteString(formatCoord(barH))
		bars.WriteString(`" fill="currentColor"/>`)
	}

	var sb strings.Builder
	sb.WriteString(`<svg class="chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)
	sb.WriteString(bars.String())
	sb.WriteString(`</svg>`)
	return sb.String()
}

// chartEmptyAxis — горизонтальная линия у нижнего края: issue без событий в
// окне графика (или без данных вовсе).
func chartEmptyAxis(w, h int) string {
	y := formatCoord(float64(h) - 0.5)
	var sb strings.Builder
	sb.WriteString(`<svg class="chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg"><line x1="0" y1="`)
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

// availabilityColorUp/Down/Empty — цвета корзин полоски доступности:
// зелёный (все проверки в корзине успешны), красный (хотя бы одна
// провалилась), серый (в корзине нет ни одной проверки — "нет данных", не
// путать с провалом). Захардкожены (а не var(--accent) и т.п.), так как
// одного currentColor тут недостаточно — нужны три разных цвета в одном
// SVG, а не один цвет из контекста, как у sparklineSVG/chartSVG.
const (
	availabilityColorUp    = "#3ecf6e"
	availabilityColorDown  = "#ff5f5f"
	availabilityColorEmpty = "#3a3f4c"
)

// availabilityBarsSVG строит полоску доступности: один прямоугольник на
// корзину uptime.Query.Bars. Пустой bars (buckets==nil/пустой слайс) рисует
// один серый прямоугольник на всю ширину — тот же принцип "нет данных не
// должно выглядеть как ошибка рендера", что и у flatlineSVG/chartEmptyAxis.
//
// bars приходят из uptime.Query.Bars (числа), поэтому собранный SVG-текст
// состоит только из чисел и трёх фиксированных цветовых констант выше —
// templ.Raw здесь безопасен по тем же причинам, что и в sparklineSVG.
func availabilityBarsSVG(bars []uptime.UptimeStat, w, h int) templ.Component {
	return templ.Raw(availabilityBarsMarkup(bars, w, h))
}

func availabilityBarsMarkup(bars []uptime.UptimeStat, w, h int) string {
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
		rects.WriteString(`" fill="`)
		rects.WriteString(availabilityBarColor(b))
		rects.WriteString(`"/>`)
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

func availabilityBarColor(b uptime.UptimeStat) string {
	switch {
	case b.Total == 0:
		return availabilityColorEmpty
	case b.OK == b.Total:
		return availabilityColorUp
	default:
		return availabilityColorDown
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
	sb.WriteString(`" fill="`)
	sb.WriteString(availabilityColorEmpty)
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

// waterfallColorOK/Error — цвет полосы спана: обычный (status == ok и нет
// привязанной ошибки) и красный (status != ok либо на спане есть событие-
// ошибка). Захардкожены, как availabilityColor* — нужны два разных цвета в
// одном SVG, одного currentColor мало.
const (
	waterfallColorOK    = "#5b8cff"
	waterfallColorError = "#ff5f5f"
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
	b.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)

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
		color := waterfallColorOK
		if isErr || (s.Status != "" && s.Status != "ok") {
			color = waterfallColorError
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
		b.WriteString(`" fill="`)
		b.WriteString(color)
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

// latencyChartWidth/Height — размер stacked-bar-графика задержек на странице
// монитора.
const (
	latencyChartWidth  = 480
	latencyChartHeight = 120
)

// latencySegmentColors — цвета сегментов stacked-bar-графика задержек, по
// порядку укладки снизу вверх: DNS, connect, TLS, TTFB. Захардкожены по той
// же причине, что и availabilityColor* выше — четыре разных цвета в одном
// SVG.
var latencySegmentColors = [4]string{"#4fb0e8", "#5b8cff", "#e0a52c", "#3ecf6e"}

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
			bars.WriteString(`" fill="`)
			bars.WriteString(latencySegmentColors[si])
			bars.WriteString(`"/>`)
		}
	}

	var sb strings.Builder
	sb.WriteString(`<svg class="latency-chart" viewBox="0 0 `)
	sb.WriteString(strconv.Itoa(w))
	sb.WriteByte(' ')
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString(`" xmlns="http://www.w3.org/2000/svg">`)
	sb.WriteString(bars.String())
	sb.WriteString(`</svg>`)
	return sb.String()
}
