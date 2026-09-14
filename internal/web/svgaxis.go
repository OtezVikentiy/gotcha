package web

import (
	"html"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
)

type chartGeom struct {
	w, h           int
	x0, x1, y0, y1 float64
}

func newChartGeom(w, h int, padL, padR, padT, padB float64) chartGeom {
	return chartGeom{
		w: w, h: h,
		x0: padL, x1: float64(w) - padR,
		y0: padT, y1: float64(h) - padB,
	}
}

func (g chartGeom) xForIndex(i, n int) float64 {
	if n <= 1 {
		return (g.x0 + g.x1) / 2
	}
	return g.x0 + float64(i)/float64(n-1)*(g.x1-g.x0)
}

func (g chartGeom) barWidth(n int) float64 {
	if n < 1 {
		return 0
	}
	return (g.x1 - g.x0) / float64(n)
}

// верх строго выше максимума — иначе самый высокий столбик упирается в рамку;
// у самой границы float64 запаса над максимумом не существует, там верх равен ему.
type yScale struct {
	top  float64
	step float64
}

func newYScale(max uint64, targetLines int) yScale {
	step := niceStep(max, targetLines)
	// (max/step+1)*step в uint64 переполняется у самой границы диапазона;
	// умножение в float64 держит верх конечным там, где uint64 просто оборачивается.
	top := (float64(max/step) + 1) * float64(step)
	return yScale{top: top, step: float64(step)}
}

// потолок делений оси; targetLines обычно 3-4 — предохранитель от денормализованного
// step, не рабочий диапазон.
const maxAxisScaleSteps = 64

// шаг из того же ряда 1/2/5×10ⁿ, что у newYScale, но без округления до целых.
func newYScaleFloat(max float64, targetLines int) yScale {
	if max <= 0 || math.IsNaN(max) || math.IsInf(max, 0) {
		return yScale{top: 1, step: 1}
	}
	step := niceStepFloat(max, targetLines)
	if step <= 0 || math.IsNaN(step) || math.IsInf(step, 0) {
		step = max
	}
	top := step
	// max у самой границы float64 (~1.8e308) — top+step переполняется в +Inf раньше,
	// чем перегонит max; останавливаемся на последнем конечном top, а не копим Inf.
	for i := 0; top <= max && i < maxAxisScaleSteps; i++ {
		next := top + step
		if math.IsInf(next, 0) || math.IsNaN(next) {
			break
		}
		top = next
	}
	if top <= max || math.IsInf(top, 0) || math.IsNaN(top) {
		top = max
	}
	return yScale{top: top, step: step}
}

func (s yScale) yFor(g chartGeom, v float64) float64 {
	if s.top <= 0 {
		return g.y1
	}
	return g.y1 - v/s.top*(g.y1-g.y0)
}

func writeFrame(sb *strings.Builder, g chartGeom) {
	axisLine(sb, g.x0, g.y0, g.x0, g.y1)
	axisLine(sb, g.x0, g.y1, g.x1, g.y1)
}

// 0.6024×кегль на руну, взят с БАЗОВОЙ (<700px) ступени chart-vb1200 — там
// отношение кегль/viewBox наибольшее из всех тиров, оценка не занижена нигде.
const svgCharWidthPerVB = 0.6024 * 57.0 / 1200.0

func svgCharWidthPx(vbW int) float64 {
	return float64(vbW) * svgCharWidthPerVB
}

func estimateTextWidth(vbW int, s string) float64 {
	return float64(utf8.RuneCountInString(s)) * svgCharWidthPx(vbW)
}

func (g chartGeom) textWidth(s string) float64 {
	return estimateTextWidth(g.w, s)
}

const yLabelGap = 6

// за этим пределом длинная подпись (патологический unit) обрезается слева —
// компромисс writeYGrid, не съеденный график.
const yLabelPadMaxShare = 0.25

// поле не меньше padL и не меньше самой широкой подписи (в пределах
// yLabelPadMaxShare); общая для chartGeom (fitYLabels) и chartBars.
func yAxisPadL(vbW int, padL float64, labels []string) float64 {
	need := 0.0
	for _, s := range labels {
		if w := estimateTextWidth(vbW, s); w > need {
			need = w
		}
	}
	need += yLabelGap
	if max := float64(vbW) * yLabelPadMaxShare; need > max {
		need = max
	}
	if need > padL {
		return need
	}
	return padL
}

// делений оси от 0 до top, тем же шагом, что рисует сетку; потолок и обрыв по
// неконечному v — свой предохранитель для yScale, собранного не через newYScaleFloat.
func yAxisTicks(s yScale) []float64 {
	if s.step <= 0 || math.IsNaN(s.step) || math.IsInf(s.step, 0) {
		return nil
	}
	limit := s.top + s.step/2
	ticks := make([]float64, 0, maxAxisScaleSteps)
	for v := 0.0; v <= limit && len(ticks) < maxAxisScaleSteps; v += s.step {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			break
		}
		ticks = append(ticks, v)
	}
	return ticks
}

// вызывать сразу после построения шкалы и до любого рисования — ось, сетка
// и данные должны лечь уже на сдвинутый x0.
func (g chartGeom) fitYLabels(s yScale, label func(v float64) string) chartGeom {
	ticks := yAxisTicks(s)
	if ticks == nil {
		return g
	}
	labels := make([]string, len(ticks))
	for i, v := range ticks {
		labels[i] = label(v)
	}
	g.x0 = yAxisPadL(g.w, g.x0, labels)
	return g
}

func writeYGrid(sb *strings.Builder, g chartGeom, s yScale, label func(v float64) string) {
	for _, v := range yAxisTicks(s) {
		y := s.yFor(g, v)
		if v > 0 {
			axisLine(sb, g.x0, y, g.x1, y)
		}
		text := label(v)
		// правый край подписи не должен заходить правее g.x0 — при нехватке
		// места приоритет здесь, а не у левого края (тот обрежется вьюбоксом).
		lx := g.x0 - yLabelGap
		if w := g.textWidth(text); lx-w < 0 {
			lx = w
		}
		if lx > g.x0 {
			lx = g.x0
		}
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(lx))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(y))
		sb.WriteString(`" text-anchor="end" dominant-baseline="middle" fill="currentColor">`)
		sb.WriteString(html.EscapeString(text))
		sb.WriteString(`</text>`)
	}
}

type xTick struct {
	x    float64
	text string
}

// первую линию не рисуем — она совпала бы с рамкой.
func writeXTicks(sb *strings.Builder, g chartGeom, ticks []xTick) {
	prevRight := math.Inf(-1)
	for i, t := range ticks {
		if i > 0 && t.x > g.x0+0.5 {
			axisLine(sb, t.x, g.y0, t.x, g.y1)
		}
		anchor, _, right, draw := xLabelPlacement(g.w, g.x0, g.x1, prevRight, t.x, t.text)
		if !draw {
			continue
		}
		prevRight = right
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(t.x))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(float64(g.h) - 7))
		sb.WriteString(`" text-anchor="` + anchor + `" fill="currentColor">`)
		sb.WriteString(html.EscapeString(t.text))
		sb.WriteString(`</text>`)
	}
}

// у краёв холста анкор растёт от края, чтобы подпись не резалась вьюбоксом;
// при наезде на предыдущую нарисованную — draw=false вместо каши.
func xLabelPlacement(vbW int, x0, x1, prevRight, x float64, text string) (anchor string, left, right float64, draw bool) {
	w := estimateTextWidth(vbW, text)
	half := w / 2

	anchor, left, right = "middle", x-half, x+half
	switch {
	case x+half > x1:
		anchor, left, right = "end", x-w, x
	case x-half < x0:
		anchor, left, right = "start", x, x+w
	}

	if left < prevRight && x+w <= x1 {
		anchor, left, right = "start", x, x+w
	}
	return anchor, left, right, left >= prevRight
}

// шаг и формат — по длине окна; метка не чаще раза в minGapPx, иначе
// подписи наезжают друг на друга.
func timeAxis(times []time.Time, xFor func(i int) float64, minGapPx float64) []xTick {
	if len(times) == 0 {
		return nil
	}
	span := times[len(times)-1].Sub(times[0])

	var gran time.Duration
	var layout string
	switch {
	case span >= 48*time.Hour:
		gran, layout = 24*time.Hour, "02.01"
	case span >= 12*time.Hour:
		gran, layout = 3*time.Hour, "15:04"
	case span >= 3*time.Hour:
		gran, layout = time.Hour, "15:04"
	default:
		gran, layout = 15*time.Minute, "15:04"
	}

	var ticks []xTick
	lastX := -1e9
	prevSlot := int64(-1)
	for i, t := range times {
		slot := t.UTC().Truncate(gran).Unix()
		if slot == prevSlot {
			continue
		}
		prevSlot = slot
		x := xFor(i)
		if x-lastX < minGapPx {
			continue
		}
		lastX = x
		ticks = append(ticks, xTick{x: x, text: t.UTC().Format(layout)})
	}
	return ticks
}

// на линейном графике наводиться не на что (линия тонкая, маркеров нет) —
// полоса перекрывает интервал целиком. Работает без JS — нативный <title>.
func writeHoverBand(sb *strings.Builder, g chartGeom, x, width float64, title string) {
	sb.WriteString(`<rect class="hover-band" x="`)
	sb.WriteString(formatCoord(x))
	sb.WriteString(`" y="`)
	sb.WriteString(formatCoord(g.y0))
	sb.WriteString(`" width="`)
	sb.WriteString(formatCoord(width))
	sb.WriteString(`" height="`)
	sb.WriteString(formatCoord(g.y1 - g.y0))
	sb.WriteString(`"><title>`)
	sb.WriteString(html.EscapeString(title))
	sb.WriteString(`</title></rect>`)
}

// деплой левее times[0] пропускается, правее — клампится к x1; столбчатые
// графики передают g с укороченным x1 — под шкалу их подписей оси X.
func writeDeployMarker(sb *strings.Builder, g chartGeom, times []time.Time, deploys []deploy.Deployment) {
	if len(times) < 2 || len(deploys) == 0 {
		return
	}
	span := times[len(times)-1].Sub(times[0]).Seconds()
	if span <= 0 {
		return
	}
	// линии рисуем все (маркер важнее подписи), подпись пропускаем при
	// наезде на предыдущую НАРИСОВАННУЮ — кап по числу не нужен.
	const labelPad = 2
	sorted := make([]deploy.Deployment, len(deploys))
	copy(sorted, deploys)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DeployedAt.Before(sorted[j].DeployedAt) })
	lastLabelRight := math.Inf(-1)
	for _, d := range sorted {
		off := d.DeployedAt.Sub(times[0]).Seconds()
		if off < 0 {
			continue
		}
		if off > span {
			off = span
		}
		x := g.x0 + off/span*(g.x1-g.x0)
		sb.WriteString(`<line class="chart-deploy-marker" x1="`)
		sb.WriteString(formatCoord(x))
		sb.WriteString(`" y1="`)
		sb.WriteString(formatCoord(g.y0))
		sb.WriteString(`" x2="`)
		sb.WriteString(formatCoord(x))
		sb.WriteString(`" y2="`)
		sb.WriteString(formatCoord(g.y1))
		sb.WriteString(`"><title>`)
		sb.WriteString(html.EscapeString(d.Version + " · " + d.DeployedAt.UTC().Format("02.01 15:04")))
		sb.WriteString(`</title></line>`)

		w := g.textWidth(d.Version)
		// start (вправо от линии); у правого края, где текст вылез бы за x1, — end.
		anchor, lx := "start", x+labelPad
		left, right := lx, lx+w
		if right > g.x1 {
			anchor, lx = "end", x-labelPad
			left, right = lx-w, lx
		}
		if left < lastLabelRight+labelPad {
			continue
		}
		lastLabelRight = right
		sb.WriteString(`<text class="chart-deploy-label" text-anchor="` + anchor + `" x="`)
		sb.WriteString(formatCoord(lx))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(g.y0 + 10))
		sb.WriteString(`">`)
		sb.WriteString(html.EscapeString(d.Version))
		sb.WriteString(`</text>`)
	}
}

// приводит микросекунды к мс/с, чтобы на оси не было семизначных чисел.
func formatUSAxis(us float64) string {
	switch {
	case us == 0:
		// Ноль без единицы: «0µs» на оси читается как значащая величина,
		// хотя это просто начало шкалы.
		return "0"
	case us >= 1_000_000:
		return trimZero(strconv.FormatFloat(us/1_000_000, 'f', 1, 64)) + "s"
	case us >= 1_000:
		return trimZero(strconv.FormatFloat(us/1_000, 'f', 0, 64)) + "ms"
	default:
		return trimZero(strconv.FormatFloat(us, 'f', 0, 64)) + "µs"
	}
}

// используется и fitYLabels — чтобы померить подписи до рисования.
func formatCountAxis(v float64) string {
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// в отличие от formatUSAxis, вход уже в мс (проверки uptime приходят в мс).
func formatMsAxis(ms float64) string {
	switch {
	case ms == 0:
		return "0"
	case ms >= 1_000:
		return trimZero(strconv.FormatFloat(ms/1_000, 'f', 1, 64)) + "s"
	default:
		return trimZero(strconv.FormatFloat(ms, 'f', 0, 64)) + "ms"
	}
}

func trimZero(s string) string {
	s = strings.TrimSuffix(s, ".0")
	if s == "" {
		return "0"
	}
	return s
}
