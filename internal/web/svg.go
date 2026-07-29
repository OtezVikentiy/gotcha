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
// svgRoot открывает корневой <svg> с доступным ИМЕНЕМ. role="img" + aria-label
// обязательны: без них скринридер объявляет график как безымянную графику, а
// <title> внутри отдельных <rect> (тултипы для мыши) на нефокусируемых фигурах,
// как правило, не озвучивается и недостижим с клавиатуры. Имя описывает, ЧТО
// изображено; сами числа доступны в соседних таблицах и подписях осей.
func svgRoot(class string, w, h int, label string) string {
	var sb strings.Builder
	sb.WriteString(`<svg class="`)
	sb.WriteString(class)
	// Класс с ШИРИНОЙ viewBox. Кегль подписей осей задаётся в единицах viewBox,
	// а на экране он равен font-size × ширинаКарточки/ширинаViewBox — то есть
	// один и тот же font-size даёт разный размер у графиков с разным viewBox.
	// В продукте их два (720 и 1200), и правило «на класс графика» накрывало
	// только половину: у 1200 подписи оставались вдвое мельче цели. Класс
	// проставляет сам генератор — новый график получит его по построению, а не
	// по памяти автора.
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

func flamegraphSVG(ctx context.Context, root *profile.FlameNode, width int) templ.Component {
	if root == nil || root.Value == 0 {
		return templ.Raw(`<p class="empty">` + html.EscapeString(i18n.T(ctx, "profile.flame.no_data")) + `</p>`)
	}
	depth := flameDepth(root)
	height := depth * flameRowHeight
	var sb strings.Builder
	sb.WriteString(strings.TrimSuffix(svgRoot("flamegraph", width, height, i18n.T(ctx, "a11y.chart.flamegraph")), ">"))
	sb.WriteString(` font-family="monospace" font-size="10">`)
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

// flameColor — детерминированный тёплый цвет по имени функции. Диапазон hue
// намеренно начинается с янтаря (24), а не с красного: чистый красный (<20) —
// семантический цвет ошибки во всём приложении (--danger), и красноватые
// кадры флеймграфа с ним ложно перекликались. Остаётся «пламя» (янтарь →
// оранжевый → золото), но без клеша с error-red. Светлота фиксирована на 58%,
// поэтому тёмная подпись (#111) читается на любом кадре в обеих темах.
func flameColor(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	hue := int(h.Sum32()%26) + 24 // 24..49 — янтарь→оранжевый→золото, без красного
	return fmt.Sprintf("hsl(%d,70%%,58%%)", hue)
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
	sb.WriteString(svgRoot("metric-chart", w, h, i18n.T(ctx, "a11y.chart.metric")))

	// Рамка осей (левая вертикаль + нижняя горизонталь).
	sb.WriteString(`<g class="chart-axis">`)
	axisLine(&sb, x0, y0, x0, y1)
	axisLine(&sb, x0, y1, x1, y1)

	// Домен значений: данные (пропуская пустые NaN-корзины дозаполнения окна) +
	// пороги (чтобы пороговые линии попадали в область). Пустые корзины ряд
	// покрывает разрывом, а не нулём — иначе на пропусках линия падала бы в 0.
	haveData := false
	var dataMin, dataMax float64
	for _, p := range points {
		if math.IsNaN(p.V) {
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

	// Линия данных с мягкой заливкой под ней. Пустые корзины дозаполнения окна
	// (NaN) — разрыв линии (has=false), а не провал в ноль: так график
	// покрывает всё выбранное окно, но не рисует данных там, где их нет.
	linePts := make([]seriesPoint, n)
	for i, p := range points {
		x := x0
		if n > 1 {
			x = x0 + float64(i)/float64(n-1)*(x1-x0)
		}
		if math.IsNaN(p.V) {
			linePts[i] = seriesPoint{x: x, has: false}
			continue
		}
		linePts[i] = seriesPoint{x: x, y: yFor(p.V), has: true}
	}
	writeLineWithArea(&sb, linePts, y1, "#3d7bff", "gradMetric", `stroke="#3d7bff"`)

	// Полосы наведения: линия тонкая, наводиться на неё нечем, поэтому
	// подсказку ловит прозрачная полоса над своим интервалом. Значение
	// показывается в той же записи, что и подписи оси. Пустые корзины
	// пропускаем — подсказки «нет данных» не нужны.
	g := chartGeom{w: w, h: h, x0: x0, x1: x1, y0: y0, y1: y1}
	band := (x1 - x0) / float64(n)
	for i, p := range points {
		if math.IsNaN(p.V) {
			continue
		}
		x := x0
		if n > 1 {
			x = x0 + float64(i)/float64(n-1)*(x1-x0)
		}
		writeHoverBand(&sb, g, x-band/2, band,
			p.T.UTC().Format("02.01 15:04")+" — "+formatAxisValue(p.V, unit))
	}
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

// seriesPoint — точка линейного графика на холсте: координаты и признак
// наличия данных в этой корзине (has=false — пропуск).
type seriesPoint struct {
	x, y float64
	has  bool
}

// bridgeSparseGaps соединяет соседние непустые корзины ЧЕРЕЗ короткие пропуски,
// оставляя разрывы только там, где данных не было заметно дольше обычного.
//
// Сетка корзин считается по ширине окна (autoStep), а не по частоте ряда, и
// ряд легко оказывается реже сетки: метрика раз в час на 24-часовом окне
// попадает в каждую пятую 12-минутную корзину. Тогда каждая точка — одиночный
// сегмент, и линейный график вырождается в частокол отметок с заливкой в пол.
//
// Порог адаптивный: медианный шаг ряда (в корзинах) × 1.5. Это не сглаживание и
// не досочинение данных — отрезок соединяет две РЕАЛЬНЫЕ соседние точки, ровно
// как в любом линейном графике; промежуточные значения не появляются, подсказки
// по-прежнему висят только над корзинами с данными. Пропуск длиннее порога
// (простой приложения) остаётся разрывом: для мониторинга это главное.
//
// Плотный ряд (медиана = 1 корзина) не трогаем вовсе: там одиночная пустая
// корзина — настоящий провал, и он обязан читаться как разрыв.
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
			// Маркер разрыва: одной пустой точки достаточно, чтобы сегмент
			// оборвался — координаты у неё не читаются.
			out = append(out, seriesPoint{})
		}
		out = append(out, p)
		prev = i
	}
	return out
}

// writeLineWithArea рисует линию по точкам с РАЗРЫВАМИ на пропусках (has=false)
// — линия не проваливается в ноль на пустых корзинах, а прерывается — и с
// мягкой заливкой-градиентом под линией (fade к прозрачному). Прямые отрезки, а
// НЕ сплайн: сглаживание рисует значения между точками, которых не было, что
// для мониторинга недопустимо. gradID должен быть уникален на странице; при
// fillHex=="" заливка не рисуется (только линия). lineAttr — атрибуты штриха
// (class="…" или stroke="#…"). baseline — низ области заливки (обычно g.y1).
func writeLineWithArea(sb *strings.Builder, pts []seriesPoint, baseline float64, fillHex, gradID, lineAttr string) {
	if fillHex != "" {
		sb.WriteString(`<defs><linearGradient id="`)
		sb.WriteString(gradID)
		sb.WriteString(`" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="`)
		sb.WriteString(fillHex)
		sb.WriteString(`" stop-opacity="0.26"/><stop offset="1" stop-color="`)
		sb.WriteString(fillHex)
		sb.WriteString(`" stop-opacity="0"/></linearGradient></defs>`)
	}

	// Ширина отметки для ОДИНОЧНОЙ корзины: половина шага между точками.
	// Одиночную корзину нельзя нарисовать полилинией — для неё нужны две точки,
	// — и раньше она просто пропускалась. Для мониторинга это худший из
	// возможных пропусков: одиночный всплеск в тишине ровно то, что надо
	// увидеть, а подсказка при наведении рисуется на каждой корзине независимо
	// и честно сообщала «3 транзакции» там, где на графике был разрыв.
	markW := 2.0
	if len(pts) > 1 {
		if step := (pts[len(pts)-1].x - pts[0].x) / float64(len(pts)-1); step > 0 {
			markW = step * 0.6
		}
	}

	// Ряд может приходить реже сетки корзин (метрика раз в час на 12-минутном
	// шаге 24-часового окна). Тогда КАЖДАЯ непустая корзина изолирована, и без
	// моста график распадается на отдельные отметки с заливкой в пол — «лес
	// спичек» вместо линии. Мостим только короткие пропуски: см. bridgeSparseGaps.
	pts = bridgeSparseGaps(pts)

	// Идём сегментами подряд идущих точек с данными; на пропуске сегмент рвётся.
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
			// Одиночная корзина — короткая горизонтальная отметка на её
			// значении, шириной в саму корзину. Рисуется теми же атрибутами
			// штриха, что и линия, поэтому совпадает с ней по цвету и теме
			// (у p95 это class=, у метрик — stroke=, окружность с fill здесь
			// не годится).
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
// sparklineSVG — врезка-спарклайн в строке таблицы. Осей ей не даём (график
// шириной в пару сантиметров), но подсказка со сводкой нужна: без неё линия
// показывает только форму, а величины остаются неизвестными. format задаёт
// запись значения (счётчик или длительность) — nil означает голое число.
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
	writeLineWithArea(&sb, linePts, float64(h), "#3d7bff", "gradSpark", `stroke="currentColor"`)
	sb.WriteString(`</svg>`)
	return sb.String()
}

// flatlineSVG — горизонтальная линия посередине: issue без событий в окне
// спарклайна (или без данных вовсе).
func flatlineSVG(ctx context.Context, w, h int) string {
	y := formatCoord(float64(h) / 2)
	var sb strings.Builder
	sb.WriteString(svgRoot("sparkline", w, h, i18n.T(ctx, "a11y.chart.sparkline")))
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
func latencySparklineSVG(ctx context.Context, points []trace.LatencyPoint, w, h int) templ.Component {
	vals := make([]uint64, len(points))
	for i, p := range points {
		vals[i] = uint64(p.P95)
	}
	// Значения — микросекунды: в подсказке приводим к ms/s, как на осях.
	return sparklineSVG(ctx, vals, w, h, func(v uint64) string { return formatUSAxis(float64(v)) })
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
		return flatlineSVG(ctx, w, h)
	}

	g := newChartGeom(w, h, 64, 16, 26, 26)
	scale := newYScaleFloat(float64(max), 3)
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

	// Перцентили. p50 — с заливкой под линией; p95 — только линия (заливка обеих
	// дала бы мутное наложение). На пустых корзинах (Count==0) линия рвётся, а
	// не проваливается в ноль — это убирает резкие V-пики.
	p50pts := make([]seriesPoint, n)
	p95pts := make([]seriesPoint, n)
	for i, p := range points {
		x := g.xForIndex(i, n)
		has := p.Count > 0
		p50pts[i] = seriesPoint{x: x, y: scale.yFor(g, float64(p.P50)), has: has}
		p95pts[i] = seriesPoint{x: x, y: scale.yFor(g, float64(p.P95)), has: has}
	}
	writeLineWithArea(&sb, p50pts, g.y1, "#3d7bff", "gradLatP50", `class="`+perfLatencyLineClasses[0]+`"`)
	writeLineWithArea(&sb, p95pts, g.y1, "", "", `class="`+perfLatencyLineClasses[1]+`"`)

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
		return chartEmptyAxis(w, h, i18n.T(ctx, "a11y.chart.latency"))
	}

	g := newChartGeom(w, h, 48, 16, 26, 26)
	scale := newYScale(max, 3)
	n := len(points)
	barW := g.barWidth(n)
	gap := barW * 0.15

	var sb strings.Builder
	sb.WriteString(svgRoot("chart-freq", w, h, i18n.T(ctx, "a11y.chart.frequency")))

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
		return chartEmptyAxis(w, h, i18n.T(ctx, "a11y.chart.throughput"))
	}

	g := newChartGeom(w, h, 48, 16, 26, 26)
	scale := newYScale(max, 3)
	n := len(buckets)
	barW := g.barWidth(n)
	gap := barW * 0.15

	var sb strings.Builder
	sb.WriteString(svgRoot("chart-freq", w, h, i18n.T(ctx, "a11y.chart.frequency")))

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
	sb.WriteString(svgRoot("chart-freq", w, h, i18n.T(ctx, "a11y.chart.frequency")))

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

	// Вертикальная сетка и подписи — по границам суток (шаг корзины меньше
	// суток, подписывать каждую корзину нечитаемо; привязку ко времени даёт
	// день). На длинном окне (30 дней) суток слишком много и метки наезжают,
	// поэтому целимся примерно в targetDayLabels равномерных подписей: сначала
	// собираем индексы границ суток, затем берём каждую k-ю. Так ось «дышит»
	// на любом окне (7д → все дни, 30д → ~каждый 4-й), а не прореживается
	// по пикселям неравномерно.
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
	for j, idx := range dayIdx {
		if j%k != 0 {
			continue
		}
		x := x0 + float64(idx)*barW
		if idx > 0 {
			axisLine(&sb, x, y0, x, y1)
		}
		// У краёв холста подпись прижимаем к своей стороне (как в writeXTicks):
		// центрированная метка первого дня у левой оси наезжала на подпись «0»
		// оси Y и рамку.
		anchor := "middle"
		switch {
		case x > x1-24:
			anchor = "end"
		case x < x0+24:
			anchor = "start"
		}
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(x))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(float64(h) - 7))
		sb.WriteString(`" text-anchor="` + anchor + `" fill="currentColor">`)
		sb.WriteString(html.EscapeString(points[idx].T.UTC().Format("02.01")))
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
// label передаёт ВЫЗЫВАЮЩИЙ: эта заглушка обслуживает три разных графика
// (задержка, throughput, Web Vital), и зашитое имя одного из них было бы враньём
// для остальных двух.
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

// availabilityBarsWidth/Height — размер полоски доступности в списке
// мониторов и на странице монитора (план 4, задача 2): по умолчанию 24
// корзины (например, часовые за последние 24 часа).
const (
	availabilityBarsWidth  = 192
	availabilityBarsHeight = 24
)

// Классы корзин полоски доступности: зелёная (все проверки в корзине
// успешны), жёлтая (большинство успешно, но были сбои — «иногда
// постреливает»), красная (большинство проверок провалилось), серая (в
// корзине нет ни одной проверки — "нет данных", не путать с провалом). Цвет
// назначает app.css из токенов, поэтому полоска следует теме. Одного
// currentColor тут мало — нужны разные цвета в одном SVG, а не один цвет из
// контекста, как у sparklineSVG/chartSVG.
const (
	availabilityClassUp      = "bar-up"
	availabilityClassPartial = "bar-partial"
	availabilityClassDown    = "bar-down"
	availabilityClassEmpty   = "bar-empty"
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
	sb.WriteString(svgRoot("availability-bars", w, h, i18n.T(ctx, "a11y.chart.availability")))
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
		// Большинство проверок в корзине провалилось.
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

func availabilityEmptyBarsSVG(ctx context.Context, w, h int) string {
	var sb strings.Builder
	sb.WriteString(svgRoot("availability-bars", w, h, i18n.T(ctx, "a11y.chart.availability")))
	sb.WriteString(`<rect x="0" y="0" width="`)
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
			points[0].T.UTC().Format("02.01") + " – " + last.T.UTC().Format("02.01") +
				" · min " + format(lo) + " · max " + format(hi) + " · " + format(last.P75)))
		sb.WriteString(`</title>`)
	}
	writeLineWithArea(&sb, linePts, float64(h), "#3d7bff", "gradVital", `stroke="currentColor"`)
	sb.WriteString(`</svg>`)
	return sb.String()
}

// latencyChartWidth/Height — размер stacked-bar-графика задержек на странице
// монитора.
const (
	latencyChartWidth  = 720
	latencyChartHeight = 160
)

// latencySegmentClasses — классы сегментов stacked-bar-графика задержек, по
// порядку укладки снизу вверх: DNS, connect, TLS, TTFB. Цвет назначает
// app.css из токенов по той же причине, что и availabilityClass* выше —
// четыре разных цвета в одном SVG, одного currentColor мало.
var latencySegmentClasses = [4]string{"seg-dns", "seg-connect", "seg-tls", "seg-ttfb"}

// latencyCapClass — метка выброса над столбиком: час, чей средний total не
// влез в шкалу (медленно/таймаут). Красная, чтобы читаться как событие, а не
// как обычная фаза.
const latencyCapClass = "seg-cap"

// latencySegmentNames — подписи фаз для подсказки. Названия технические
// (DNS, TCP, TLS, TTFB) и одинаковы во всех языках, поэтому в каталог не
// выносятся.
var latencySegmentNames = [4]string{"DNS", "TCP", "TLS", "TTFB"}

// latencyStackedSVG строит stacked-bar-график по фазам таймингов
// (DNS/TCP/TLS/TTFB) на точку временного ряда uptime.Query.Latency, с осями,
// сеткой (в мс), метками времени и подсказкой на каждый час — тем же каркасом
// (svgaxis.go), что и графики перфоманса.
//
// Шкалу задаёт максимум СУММЫ рисуемых фаз, а НЕ AvgTotalMs. У часа с
// таймаутом фазы ≈ 0 (соединения/TTFB не было), зато total ≈ 30000мс: если
// нормировать на total, здоровые часы (90–150мс) схлопываются в невидимые
// огрызки у дна — ровно так график и стал нечитаемым. Час, чей средний total
// вылез за шкалу (медленно/таймаут), помечаем красной меткой сверху
// (latencyCapClass), но саму шкалу он не ломает. Сумма фаз обычно меньше total
// (остаток — тело ответа и прочий оверхед вне разбивки); полный total виден в
// подсказке.
//
// points приходят из uptime.Query.Latency (числа), поэтому собранный
// SVG-текст состоит только из чисел и фиксированных цветов —
// templ.Raw здесь безопасен по тем же причинам, что и в sparklineSVG.
func latencyStackedSVG(ctx context.Context, points []uptime.LatencyPoint, w, h int) templ.Component {
	return templ.Raw(latencyStackedMarkup(ctx, points, w, h))
}

func latencyStackedMarkup(ctx context.Context, points []uptime.LatencyPoint, w, h int) string {
	var maxPhase uint32
	for _, p := range points {
		if sum := p.AvgDNSMs + p.AvgConnectMs + p.AvgTLSMs + p.AvgTTFBMs; sum > maxPhase {
			maxPhase = sum
		}
	}
	if len(points) == 0 || maxPhase == 0 {
		return chartEmptyAxis(w, h, i18n.T(ctx, "a11y.chart.vital"))
	}

	g := newChartGeom(w, h, 48, 16, 26, 26)
	scale := newYScaleFloat(float64(maxPhase), 3)
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
		// segGap — тонкий зазор между фазами (единицы viewBox): сегмент рисуется
		// на segGap короче сверху, обнажая фон карточки, — фазы разделяются не
		// только цветом. Для очень тонких сегментов зазор пропускается, иначе
		// они бы исчезли.
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

		// Метка выброса: средний total выше видимой шкалы (медленный час или
		// таймаут). Треугольник у верхней рамки над своим слотом.
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

		// Полоса наведения на весь слот: подсказка появляется в любом месте над
		// часом, даже если фазы нулевые (таймаут).
		title := p.T.UTC().Format("02.01 15:04")
		for si, ms := range segments {
			title += " · " + latencySegmentNames[si] + " " + strconv.FormatUint(uint64(ms), 10) + "ms"
		}
		title += " · " + i18n.T(ctx, "chart.total") + " " + strconv.FormatUint(uint64(p.AvgTotalMs), 10) + "ms"
		if capped {
			title += " · " + i18n.T(ctx, "uptime.chart.over_scale")
		}
		writeHoverBand(&sb, g, slotX-gap/2, barW, title)
	}
	sb.WriteString(`</svg>`)
	return sb.String()
}
