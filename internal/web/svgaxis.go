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

// svgCharWidthPerVB — ширина одной руны подписи оси на единицу ширины
// viewBox. Кегль подписей задаёт CSS по классу chart-vb<N> (svgRoot,
// app.css), и ступени кегля пропорциональны N («значения для N = значения
// для 720 × N/720»), поэтому ширина руны в единицах viewBox тоже
// пропорциональна N: одна константа накрывает все тиры (720/960/1200) по
// построению, а не один из них. Та же методика, что и обрезка подписей
// флеймграфа (truncateRunes в svg.go) — единственная оценка ширины текста в
// пакете, переиспользуется здесь и в xLabelPlacement/writeDeployMarker.
//
// Калибровка: замер getComputedTextLength() на --font-mono даёт ширину руны
// ≈0.6024×кегль. Опорная ступень — @media(min-width:700px): САМЫЙ крупный
// кегль из тех, при которых график рисуется в своей естественной ширине
// (15 у chart-vb720 → 9.04 на руну; 25 у chart-vb1200 → 15.06). Прежняя
// константа 6.0 была снята с самой МЕЛКОЙ ступени (chart-vb720 при ≥1300px,
// кегль 10) и на тире chart-vb1200 при 1000-1300px занижала ширину руны
// вдвое: подписи оси Y «200ms» резались слева, подписи версий деплоя
// слипались (аудит 09-04 K9-4, повтор 08-27 P1-6/P1-7). На более широких
// окнах кегль мельче, и оценка консервативна: подписи разрежены чуть
// сильнее, чем строго необходимо, но никогда не наезжают. Ступень без
// @media (<700px) сюда не входит: там SVG держит min-width 480px и
// собственный кегль 40/32 (app.css, .issue-chart и семья), а
// прореживание ниже этой ширины — задача CSS, не генератора: SVG рисуется
// один раз на сервере и не знает ширину окна. Соответствие ступени и
// константы держит TestSvgCharWidthMatchesCSSTier (css_chart_vb_test.go).
const svgCharWidthPerVB = 0.6024 * 15.0 / 720.0

// svgCharWidthPx — ширина руны подписи для графика шириной vbW единиц.
func svgCharWidthPx(vbW int) float64 {
	return float64(vbW) * svgCharWidthPerVB
}

// estimateTextWidth — приближённая ширина подписи в единицах viewBox графика
// шириной vbW (см. svgCharWidthPerVB).
func estimateTextWidth(vbW int, s string) float64 {
	return float64(utf8.RuneCountInString(s)) * svgCharWidthPx(vbW)
}

// textWidth — estimateTextWidth для холста g.
func (g chartGeom) textWidth(s string) float64 {
	return estimateTextWidth(g.w, s)
}

// yLabelGap — зазор между правым краем подписи оси Y и самой осью.
const yLabelGap = 6

// yLabelPadMaxShare — предел, до которого левое поле растёт под подписи оси
// Y: четверть холста (≈20 рун при любой ширине, потому что и ширина руны, и
// предел пропорциональны vbW). Дальше подпись (патологически длинный unit из
// OTLP) обрезается слева — компромисс writeYGrid, а не съеденный график.
const yLabelPadMaxShare = 0.25

// yAxisPadL — левое поле под подписи оси Y: не меньше padL из геометрии и не
// меньше самой широкой подписи с зазором yLabelGap (в пределах
// yLabelPadMaxShare). Поля 48-64, заданные вызывающими под мелкий тир,
// на тире chart-vb1200 при 700-1300px не вмещали «200ms» (75 единиц) —
// подпись резалась левым краем вьюбокса (K9-4). Общая для chartGeom
// (fitYLabels) и chartBars (svg.go), у которого своя шкала.
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

// fitYLabels — копия g с левым полем, вмещающим все подписи шкалы s
// (yAxisPadL). Вызывается сразу после построения шкалы и ДО любого
// рисования: ось, сетка и данные должны лечь уже на сдвинутый x0.
func (g chartGeom) fitYLabels(s yScale, label func(v float64) string) chartGeom {
	if s.step <= 0 {
		return g
	}
	var labels []string
	for v := 0.0; v <= s.top+s.step/2; v += s.step {
		labels = append(labels, label(v))
	}
	g.x0 = yAxisPadL(g.w, g.x0, labels)
	return g
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
		// Это компромисс, не устранение: когда g.textWidth(text) > g.x0,
		// оба ограничения одновременно неудовлетворимы, и левый край ВСЁ
		// РАВНО обрезается вьюбоксом (пользователь видит подпись, обрезанную
		// слева) — просто без наложения на график. Порог начала обрезки —
		// длина подписи в рунах > g.x0/svgCharWidthPx(g.w); при поле,
		// расширенном fitYLabels до предела yLabelPadMaxShare, это от ~20 рун
		// (например "12.3K megabytes/sec" из
		// TestWriteYGridRightEdgeStaysOutOfPlotArea).
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
// draw=false в writeXTicks достижимо, но редко: текст там ограничен
// форматами "02.01"/"15:04" (максимум 5 рун), а зазор между тиками
// гарантирован timeAxis'ом (minGapPx=70 во всех 5 местах вызова в svg.go).
// По калибровке svgCharWidthPerVB 5 рун — это 45 единиц на chart-vb720 и 75
// на chart-vb1200: на широком холсте подпись шире минимального зазора, и
// если timeAxis выдал тики вплотную (окно ~48ч с шагом 3ч на 1200), вторая
// из пары эскалируется в "start" или подавляется — вместо каши. На типичных
// окнах (тики на границах часов/суток идут через 100+ единиц) подавления
// нет. Оценка снята с самой крупной ступени естественной ширины
// (≥700px); на ступени без @media (<700px) кегль ещё крупнее, и там
// подписи держит CSS (min-width холста и свой кегль), не генератор.
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
	// Подпись версии: ширина считается по калибровке тира (textWidth), а не
	// фиксированным порогом — при кегле 22-25 (chart-vb1200 на 700-1300px)
	// «v1.2.2» занимает 80-90 единиц, и прежние константы (порог у края 40,
	// зазор 44, снятые с мелкого тира) давали слипшиеся «v1.2.2v1.2.3»
	// (аудит 09-04 K9-4, повтор 08-27 P1-7). Антиколлизия: линии рисуем ВСЕ
	// (маркер важнее подписи), а подпись пропускаем, если её левый край
	// наезжает на предыдущую НАРИСОВАННУЮ. Так кап по числу не нужен: на любой
	// плотности остаётся читаемый разреженный ряд. Деплои обходятся в порядке
	// времени, чтобы «предыдущая» была соседней слева, а не той, что раньше
	// встретилась в списке.
	const labelPad = 2
	sorted := make([]deploy.Deployment, len(deploys))
	copy(sorted, deploys)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DeployedAt.Before(sorted[j].DeployedAt) })
	lastLabelRight := math.Inf(-1)
	for _, d := range sorted {
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

		w := g.textWidth(d.Version)
		// Якорь подписи: start (вправо от линии); у правого края, где текст
		// вылез бы за x1, — end (влево от линии), как и writeXTicks.
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

// formatCountAxis — счётчик на оси (throughput, гистограмма, объём логов):
// целое без дробной части. Одна функция на три графика — она же нужна
// fitYLabels, чтобы померить подписи до рисования.
func formatCountAxis(v float64) string {
	return strconv.FormatFloat(v, 'f', 0, 64)
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
