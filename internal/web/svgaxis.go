package web

import (
	"html"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
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

// svgCharWidthPx — грубая оценка ширины одной руны подписи в единицах
// viewBox. Та же методика, что и обрезка подписей флеймграфа
// (truncateRunes в svg.go: строка обрезается по ширине w/svgCharWidthPx) —
// единственная оценка ширины текста в пакете, переиспользуется здесь и в
// xLabelPlacement, а не заводится заново под каждый генератор.
//
// Калибровка ОДНОГО тира, не средняя по всем: замер getComputedTextLength()
// на --font-mono даёт ширину руны ≈0.6024×кегль, а кегль подписей зависит от
// вьюпорта и класса chart-vb<N> (см. svgRoot и app.css). 6.0 совпадает с
// chart-vb720 при вьюпорте ≥1300px (кегль 10px → 6.022, расхождение 0.4%) —
// это САМЫЙ мелкий кегль из всех тиров. На остальных тирах реальная ширина
// руны БОЛЬШЕ, местами в разы (chart-vb1200 базовый тир: кегль 57px → 34.32
// на руну, ×5.7 от заложенных 6.0). Любой вывод в этом файле, посчитанный
// через estimateTextWidth, верен в пределах этой калибровки (десктопный
// тир) и не переносится на узкие вьюпорты без неё — см. оговорку у
// xLabelPlacement. Развести оценку по тирам — отдельная задача (SVG рисуется
// один раз на сервере и не знает, каким CSS-тиром его покажут), не патч.
const svgCharWidthPx = 6.0

// estimateTextWidth — приближённая ширина подписи в единицах viewBox (см.
// svgCharWidthPx).
func estimateTextWidth(s string) float64 {
	return float64(utf8.RuneCountInString(s)) * svgCharWidthPx
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
		text := label(v)
		// Подпись растёт влево от x (text-anchor=end): при малом x0 и
		// длинной подписи левый край уходит за x=0 и режется вьюбоксом —
		// прижимаем x так, чтобы левый край был не меньше 0. Но unit в
		// подписи приходит из MetricInfo.Unit (внешняя OTLP-строка без
		// ограничения длины), и прижатый x для достаточно длинной подписи
		// может оказаться ПРАВЕЕ g.x0 — тогда правый край текста (сам x,
		// якорь end) залезает В область графика, поверх сетки и данных, что
		// строго хуже обрезки вьюбоксом. Правый край не должен заходить
		// правее g.x0 ни при каких обстоятельствах: если ширины подписи не
		// хватает и на то, и на другое (случай патологически длинного
		// unit), приоритет — не залезать на график, а не идеальный левый
		// край.
		//
		// Это компромисс, не устранение: когда estimateTextWidth(text) > g.x0,
		// оба ограничения одновременно неудовлетворимы, и левый край ВСЁ
		// РАВНО обрезается вьюбоксом (пользователь видит подпись, обрезанную
		// слева) — просто без наложения на график. Порог начала обрезки —
		// длина подписи в рунах > g.x0/svgCharWidthPx; при типичном g.x0=58
		// (padL из svg.go/svg_slo.go) это от 10 рун (например
		// "kilobytes/sec" или "12.3K megabytes" из TestWriteYGridRightEdgeStaysOutOfPlotArea).
		lx := g.x0 - 6
		if w := estimateTextWidth(text); lx-w < 0 {
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

// xTick — вертикальная метка оси времени.
type xTick struct {
	x    float64
	text string
}

// writeXTicks рисует вертикальные линии сетки и подписи под ними. Первую
// линию не рисуем — она совпала бы с рамкой.
func writeXTicks(sb *strings.Builder, g chartGeom, ticks []xTick) {
	prevRight := math.Inf(-1)
	for i, t := range ticks {
		if i > 0 && t.x > g.x0+0.5 {
			axisLine(sb, t.x, g.y0, t.x, g.y1)
		}
		anchor, _, right, draw := xLabelPlacement(g.x0, g.x1, prevRight, t.x, t.text)
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

// xLabelPlacement — якорь, горизонтальные границы (left, right) и признак
// draw (можно ли нарисовать подпись без наезда на предыдущую) для подписи
// тика по оси X. По умолчанию якорь "middle" (центр в x). У краёв холста
// подпись растёт «от края» — иначе центрированная метка вылезала бы за
// границу и обрезалась вьюбоксом («18:0» вместо «18:00»): x+half>x1 →
// "end", x-half<x0 → "start".
//
// Если получившийся якорь (в том числе краевой) всё равно наезжает на
// предыдущую НАРИСОВАННУЮ подпись (left<prevRight), сначала пробуем
// починить это переключением на "start" — растёт вправо, прочь от соседа —
// но только если так подпись не заедет за x1 (иначе одно наложение
// меняем на другое, не лучше). Раньше эту защиту применяла только ветка
// "start" (P1-7 — «первая подпись наезжала на вторую»); ветка "end" не
// сверялась с prevRight вовсе, хотя якорь end сдвигает левый край подписи
// ЕЩЁ левее среднего — риск наезда у него не ниже, а выше. Если после этого
// подпись всё равно наезжает (например у самого правого тика одновременно
// не хватает места и до x1, и до соседа слева — анкор поменять уже
// невозможно, не нарушив x1), draw=false: подпись не рисуется вовсе —
// нечитаемая каша хуже пропущенной подписи, тот же приём, что и у
// writeDeployMarker. Общая для writeXTicks (тики времени) и подписей дней в
// chartBars (svg.go) — раньше это были две копии одного порога, которые
// могли разойтись.
//
// draw=false в writeXTicks — защита на будущее, а не наблюдаемое сегодня
// поведение: текст там ограничен форматами "02.01"/"15:04" (максимум 5 рун),
// а зазор между тиками гарантирован timeAxis'ом (minGapPx=70 во всех 5
// местах вызова в svg.go). НО этот вывод верен ТОЛЬКО в пределах калибровки
// svgCharWidthPx (см. её докблок) — десктопный тир chart-vb720 при вьюпорте
// ≥1300px, где ширина 5-рунной подписи ≈30 (нужно 60 при зазоре 70,
// запас есть). На более узких вьюпортах CSS даёт кегль КРУПНЕЕ (у базового
// тира chart-vb1200 — в 5.7 раза), реальная ширина той же подписи «20.08»
// там ≈171, а не 30, и вывод «недостижимо» на неё не переносится: SVG
// рисуется один раз на сервере и не знает, каким тиром его покажут (та же
// причина, по которой развести оценку по тирам — отдельная задача, а не
// патч в этом файле). Условие также перестанет быть недостижимым в пределах
// калибровки, если появится вызывающий с меньшим зазором или более длинными
// подписями — тогда защита сработает по построению, без правки этой
// функции.
//
// В chartBars (подписи дней) draw=false, наоборот, ДОСТИЖИМО и наблюдается
// на проде: пресет «24h» (issueChartBuckets=56, autoStep(24h, 5m, 0, 56) ≈
// 25м42с на шаг, ~56 бакетов на chartWidth=1200 → barW≈20.5px). У 24-часового
// окна не больше 2 границ суток, поэтому k=1 и НЕ прореживается ни одна —
// защита на «редко рендерится» здесь не работает, в отличие от общего
// случая. Когда tr.From (начало окна) попадает в ~26-минутный интервал перед
// полуночью UTC (ровно ширина шага — столько именно и остаётся до полуночи
// внутри первого бакета), dayIdx[0]=0 и dayIdx[1]=1 — СОСЕДНИЕ индексы:
// зазор между первой и второй подписью дня — barW≈20.5px, меньше нужных 30
// даже после эскалации второй в "start". Вторая подпись (день, начавшийся
// только что) не рисуется — вместо неё пропуск. Это происходит КАЖДЫЕ сутки
// на ~26 минут вокруг полуночи UTC у любого, кто в это время смотрит issue с
// пресетом «24h». Поведение целевое, не брак: на BASE в этом же сценарии обе
// подписи рисовались, но налезали друг на друга (нечитаемая каша) — принцип
// «каша хуже пропущенной подписи», задекларированный выше, здесь и
// применяется. Изменение поведения (было: две налезающие подписи; стало:
// одна) — заметное, см. CHANGELOG.
func xLabelPlacement(x0, x1, prevRight, x float64, text string) (anchor string, left, right float64, draw bool) {
	w := estimateTextWidth(text)
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

// writeDeployMarker рисует вертикальные пунктирные линии в моменты деплоя
// (C5): по каждому графику проекта видно, когда была выкладка, чтобы
// сопоставить её с изменением метрик/латентности/трафика. Позиция берётся по
// СРЕЗУ timestamp'ов точек графика, а не по окну хендлера: деплой между
// times[0] и times[len-1] попадает ровно на ту x-координату, где стоит точка с
// таким же временем. Деплой ЛЕВЕЕ times[0] пропускается (маркер за левой рамкой
// вводил бы в заблуждение), а хвостовой — между times[len-1] и концом окна
// хендлера (to=now отстаёт от последней корзины на ~step) — клампится к правому
// краю x1: последняя корзина агрегирует данные до now, поэтому «только что
// выкатили» садится на правый край графика, а не пропадает.
//
// Для линейных графиков вызывающий передаёт g как есть (times[0]→g.x0,
// times[last]→g.x1). Для столбчатых — копию g с x1, укороченным до левого края
// последнего слота (g.x0 + (n-1)*barW), чтобы маркер лёг в ту же шкалу
// времени, что и подписи оси X (writeXTicks строит их по g.x0 + i*barW).
func writeDeployMarker(sb *strings.Builder, g chartGeom, times []time.Time, deploys []deploy.Deployment) {
	if len(times) < 2 || len(deploys) == 0 {
		return
	}
	span := times[len(times)-1].Sub(times[0]).Seconds()
	if span <= 0 {
		return
	}
	// Порог у правого края: подпись версии свежего деплоя (маркер у x1) с якорем
	// start вылезла бы за холст и обрезалась — прижимаем её к правому краю, как
	// writeXTicks поступает с меткой последнего тика.
	const labelEdgePx = 40
	// Антиколлизия подписей: на плотном окне выкладок версии наехали бы друг на
	// друга в сплошную кашу. Линии рисуем ВСЕ (маркер важнее подписи), а подпись
	// пропускаем, если её x ближе minGap к предыдущей НАРИСОВАННОЙ. Так кап по
	// числу не нужен: на любой плотности остаётся читаемый разреженный ряд.
	const labelMinGapPx = 44
	lastLabelX := -1e9
	for _, d := range deploys {
		off := d.DeployedAt.Sub(times[0]).Seconds()
		if off < 0 {
			// Деплой левее окна графика: точки с таким временем на графике нет,
			// маркер за левой рамкой вводил бы в заблуждение — пропускаем.
			continue
		}
		if off > span {
			// Деплой в хвосте (times[last], to]: последняя корзина агрегирует
			// данные до now, поэтому «только что выкатил» семантически лежит на
			// правом краю — клампим к x1, иначе ядро фичи не рисовалось бы.
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

		gap := x - lastLabelX
		if gap < 0 {
			gap = -gap
		}
		if gap < labelMinGapPx {
			continue
		}
		lastLabelX = x
		// Якорь подписи: у правого края разворачиваем на end (текст влево от
		// линии), иначе start (вправо) — сверено с writeXTicks.
		anchor, lx := "start", x+2
		if x > g.x1-labelEdgePx {
			anchor, lx = "end", x-2
		}
		sb.WriteString(`<text class="chart-deploy-label" text-anchor="` + anchor + `" x="`)
		sb.WriteString(formatCoord(lx))
		sb.WriteString(`" y="`)
		sb.WriteString(formatCoord(g.y0 + 10))
		sb.WriteString(`">`)
		sb.WriteString(html.EscapeString(d.Version))
		sb.WriteString(`</text>`)
	}
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

// formatMsAxis — длительность оси для рядов в миллисекундах (проверки uptime
// приходят уже в мс, в отличие от трасс в микросекундах у formatUSAxis).
// Крупные значения приводятся к секундам, чтобы на оси не было «30000».
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
