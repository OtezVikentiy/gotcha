package web

import (
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
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
