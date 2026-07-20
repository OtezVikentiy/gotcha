package web

import (
	"html"
	"strconv"
	"strings"
	"time"
)

// Общий каркас графиков: поля под подписи, «круглая» шкала Y с сеткой и
// метки времени по X. Раньше эта разметка существовала только у графика
// частоты на странице issue, а латентность, трафик и гистограмма рисовались
// голыми линиями и столбиками — по ним нельзя было прочитать ни величину, ни
// время. Вынесено сюда, чтобы четыре графика не разъехались в деталях.

// chartGeom — область рисования внутри холста: поля отведены под подписи оси
// Y слева и оси X снизу.
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

// xForIndex — координата i-й точки из n при отрисовке линией (первая точка на
// левой границе, последняя на правой).
func (g chartGeom) xForIndex(i, n int) float64 {
	if n <= 1 {
		return g.x0
	}
	return g.x0 + float64(i)/float64(n-1)*(g.x1-g.x0)
}

// barWidth — ширина слота столбика при отрисовке столбиками (n слотов на всю
// область).
func (g chartGeom) barWidth(n int) float64 {
	if n < 1 {
		return 0
	}
	return (g.x1 - g.x0) / float64(n)
}

// yScale — шкала значений: «круглый» шаг и верх строго выше максимума. Запас
// сверху нужен, чтобы самый высокий столбик не упирался в рамку — иначе
// график читается как сплошной забор (см. chartBars).
type yScale struct {
	top  float64
	step float64
}

// newYScale строит шкалу для целочисленных величин (счётчики).
func newYScale(max uint64, targetLines int) yScale {
	step := niceStep(max, targetLines)
	return yScale{top: float64((max/step + 1) * step), step: float64(step)}
}

// newYScaleFloat строит шкалу для дробных величин (длительности в мкс): шаг
// берётся из того же ряда 1/2/5×10ⁿ, но без округления до целых.
func newYScaleFloat(max float64, targetLines int) yScale {
	if max <= 0 {
		return yScale{top: 1, step: 1}
	}
	step := niceStepFloat(max, targetLines)
	top := step
	for top <= max {
		top += step
	}
	return yScale{top: top, step: step}
}

func (s yScale) yFor(g chartGeom, v float64) float64 {
	if s.top <= 0 {
		return g.y1
	}
	return g.y1 - v/s.top*(g.y1-g.y0)
}

// writeFrame рисует левую вертикаль и базовую линию — рамку, относительно
// которой читаются подписи.
func writeFrame(sb *strings.Builder, g chartGeom) {
	axisLine(sb, g.x0, g.y0, g.x0, g.y1)
	axisLine(sb, g.x0, g.y1, g.x1, g.y1)
}

// writeYGrid рисует горизонтальные линии шкалы и подписывает каждую. label
// получает значение уровня и возвращает текст с единицей измерения —
// пользователь не должен догадываться, в чём измеряется ось.
func writeYGrid(sb *strings.Builder, g chartGeom, s yScale, label func(v float64) string) {
	for v := 0.0; v <= s.top+s.step/2; v += s.step {
		y := s.yFor(g, v)
		if v > 0 {
			axisLine(sb, g.x0, y, g.x1, y)
		}
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(g.x0 - 6))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(y))
		sb.WriteString(`" text-anchor="end" dominant-baseline="middle" fill="currentColor">`)
		sb.WriteString(html.EscapeString(label(v)))
		sb.WriteString(`</text>`)
	}
}

// xTick — вертикальная метка оси времени.
type xTick struct {
	x    float64
	text string
}

// writeXTicks рисует вертикальные линии сетки и подписи под ними. Первую
// линию не рисуем — она совпала бы с рамкой.
func writeXTicks(sb *strings.Builder, g chartGeom, ticks []xTick) {
	for i, t := range ticks {
		if i > 0 && t.x > g.x0+0.5 {
			axisLine(sb, t.x, g.y0, t.x, g.y1)
		}
		// У края холста подпись прижимаем к своей стороне: центрированная
		// метка последнего тика вылезала за правую границу и обрезалась
		// («18:0» вместо «18:00»).
		anchor := "middle"
		switch {
		case t.x > g.x1-24:
			anchor = "end"
		case t.x < g.x0+24:
			anchor = "start"
		}
		sb.WriteString(`<text x="`)
		sb.WriteString(formatCoord(t.x))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(float64(g.h) - 7))
		sb.WriteString(`" text-anchor="` + anchor + `" fill="currentColor">`)
		sb.WriteString(html.EscapeString(t.text))
		sb.WriteString(`</text>`)
	}
}

// timeAxis подбирает шаг и формат меток по длине окна: на сутках и меньше —
// часы, на более длинном окне — дни. Метка ставится на границе шага, но не
// чаще, чем раз в minGapPx пикселей, иначе подписи наезжают друг на друга.
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

// hoverBand — прозрачная полоса поверх интервала с подсказкой. На линейном
// графике наводиться не на что: линия тонкая, а точек-маркеров нет. Полоса
// перекрывает интервал целиком, поэтому подсказка появляется в любом месте
// над своим участком. Работает без JS — это нативный <title>.
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

// formatUSAxis — длительность для подписи оси: микросекунды приводятся к
// миллисекундам или секундам, чтобы на оси не было семизначных чисел.
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

func trimZero(s string) string {
	s = strings.TrimSuffix(s, ".0")
	if s == "" {
		return "0"
	}
	return s
}
