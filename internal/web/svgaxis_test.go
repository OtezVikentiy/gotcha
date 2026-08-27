package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// textTag — атрибуты одного <text> элемента, разобранные из сгенерированного
// SVG, для проверки геометрии подписей числами (а не наличием подстроки).
type textTag struct {
	x      float64
	anchor string
}

var textTagRe = regexp.MustCompile(`<text([^>]*)>`)
var textAttrXRe = regexp.MustCompile(`x="([-\d.]+)"`)
var textAttrAnchorRe = regexp.MustCompile(`text-anchor="(\w+)"`)

func parseTextTags(t *testing.T, svg string) []textTag {
	t.Helper()
	var tags []textTag
	for _, m := range textTagRe.FindAllStringSubmatch(svg, -1) {
		attrs := m[1]
		xm := textAttrXRe.FindStringSubmatch(attrs)
		if xm == nil {
			t.Fatalf("<text> без атрибута x: %q", attrs)
		}
		x, err := strconv.ParseFloat(xm[1], 64)
		if err != nil {
			t.Fatalf("x=%q не число: %v", xm[1], err)
		}
		anchor := "start" // SVG-дефолт при отсутствии text-anchor
		if am := textAttrAnchorRe.FindStringSubmatch(attrs); am != nil {
			anchor = am[1]
		}
		tags = append(tags, textTag{x: x, anchor: anchor})
	}
	return tags
}

// labelBounds — горизонтальные границы подписи по её якорю: "start" растёт
// вправо от x, "end" — влево, "middle" — поровну в обе стороны.
func labelBounds(x float64, anchor string, width float64) (left, right float64) {
	switch anchor {
	case "start":
		return x, x + width
	case "end":
		return x - width, x
	default:
		return x - width/2, x + width/2
	}
}

// TestFormatUSAxis — подпись оси несёт единицу измерения: пользователь не
// должен гадать, микросекунды это или миллисекунды. Ноль — исключение, «0µs»
// читалось бы как значащая величина.
func TestFormatUSAxis(t *testing.T) {
	cases := []struct {
		us   float64
		want string
	}{
		{0, "0"},
		{450, "450µs"},
		{50_000, "50ms"},
		{1_500_000, "1.5s"},
		{2_000_000, "2s"},
	}
	for _, c := range cases {
		if got := formatUSAxis(c.us); got != c.want {
			t.Errorf("formatUSAxis(%v) = %q, want %q", c.us, got, c.want)
		}
	}
}

// TestTimeAxisGranularity — шаг меток выбирается по длине окна: на неделе
// подписывать часы бессмысленно, на трёх часах — сутки.
func TestTimeAxisGranularity(t *testing.T) {
	mk := func(n int, step time.Duration) []time.Time {
		base := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
		out := make([]time.Time, n)
		for i := range out {
			out[i] = base.Add(time.Duration(i) * step)
		}
		return out
	}
	xFor := func(i int) float64 { return float64(i) * 100 }

	week := timeAxis(mk(14, 12*time.Hour), xFor, 10)
	if len(week) == 0 || !strings.Contains(week[0].text, ".") {
		t.Errorf("на недельном окне ожидались метки-даты, got %+v", week)
	}
	day := timeAxis(mk(24, time.Hour), xFor, 10)
	if len(day) == 0 || !strings.Contains(day[0].text, ":") {
		t.Errorf("на суточном окне ожидались метки-часы, got %+v", day)
	}
}

// TestTimeAxisRespectsMinGap — метки не ставятся чаще, чем раз в minGapPx:
// иначе подписи наезжают друг на друга.
func TestTimeAxisRespectsMinGap(t *testing.T) {
	base := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	times := make([]time.Time, 48)
	for i := range times {
		times[i] = base.Add(time.Duration(i) * time.Hour)
	}
	ticks := timeAxis(times, func(i int) float64 { return float64(i) * 5 }, 70)
	for i := 1; i < len(ticks); i++ {
		if gap := ticks[i].x - ticks[i-1].x; gap < 70 {
			t.Errorf("метки ближе минимального зазора: %v", gap)
		}
	}
}

// TestYScaleHeadroom — верх шкалы строго выше максимума, иначе самый высокий
// столбик упирается в рамку.
func TestYScaleHeadroom(t *testing.T) {
	if s := newYScale(10, 3); s.top <= 10 {
		t.Errorf("newYScale(10): top = %v, ожидался запас над максимумом", s.top)
	}
	if s := newYScaleFloat(90_000, 3); s.top <= 90_000 {
		t.Errorf("newYScaleFloat(90000): top = %v, ожидался запас над максимумом", s.top)
	}
}

// TestWriteYGridClampsLongLabelToCanvas — подпись оси Y растёт влево от
// x0-6 (text-anchor=end); при малом x0 и длинной подписи левый край уходил
// за x=0 и обрезался вьюбоксом (P1-6). Сценарий подобран так, что ширины
// подписи хватает и на то, чтобы прижаться к x=0, и на то, чтобы остаться
// внутри поля слева от x0 (width < x0) — оба края проверяются числами.
func TestWriteYGridClampsLongLabelToCanvas(t *testing.T) {
	g := chartGeom{w: 300, h: 100, x0: 100, x1: 290, y0: 10, y1: 90}
	s := yScale{top: 0, step: 1}         // один тик, v=0 — геометрия подписи не зависит от значения
	const longLabel = "1234567890123456" // 16 рун — шире x0-6=94, уже, чем x0=100
	var sb strings.Builder
	writeYGrid(&sb, g, s, func(float64) string { return longLabel })

	tags := parseTextTags(t, sb.String())
	if len(tags) != 1 {
		t.Fatalf("ожидалась 1 подпись оси Y, получено %d: %s", len(tags), sb.String())
	}
	tag := tags[0]
	if tag.anchor != "end" {
		t.Fatalf("якорь подписи оси Y = %q, ожидался end", tag.anchor)
	}
	width := estimateTextWidth(longLabel)
	leftEdge := tag.x - width
	if leftEdge < -0.05 {
		t.Errorf("левый край подписи оси Y = %.2f (x=%.2f, ширина=%.2f) — уходит за x=0", leftEdge, tag.x, width)
	}
	if tag.x > g.x0+0.05 {
		t.Errorf("правый край подписи оси Y = %.2f заходит правее x0=%.1f — залезает в область графика", tag.x, g.x0)
	}
	// Без прижимания x был бы g.x0-6 = 94, а левый край = 94-width = -2 —
	// чуть за вьюбоксом. С прижимом x должен ровно совпасть с шириной
	// (левый край = 0), и этой ширины (96) хватает, чтобы не залезть за x0
	// (100) — обе границы точны, не просто «в пределах».
	if math.Abs(tag.x-width) > 0.05 {
		t.Errorf("x подписи = %.2f, ожидалось %.2f (x0-6 недостаточно для этой подписи, прижато так, что левый край = 0)", tag.x, width)
	}
}

// TestWriteYGridRightEdgeStaysOutOfPlotArea — реалистичный сценарий из
// ревью: x0=58 (padL из svg.go/svg_slo.go), подпись длиннее самого x0
// (unit приходит из MetricInfo.Unit — внешняя OTLP-строка без ограничения
// длины, "12.3K megabytes" реалистична). Прижатый под левый край x (=width)
// в этом случае оказывался ПРАВЕЕ x0 — правый край подписи залезал В
// область графика поверх сетки и данных, что строго хуже обрезки вьюбоксом.
// Правый край не должен заходить правее x0 ни при каких обстоятельствах.
//
// Компромисс (записан, не подразумевается): когда ширины подписи не хватает
// ОДНОВРЕМЕННО и на левый, и на правый край, приоритет — не залезать на
// график, поэтому левый край в этом случае ВСЁ РАВНО обрезается вьюбоксом
// (пользователь видит подпись, обрезанную слева). Порог начала обрезки —
// длина подписи в рунах > x0/svgCharWidthPx = 58/6 ≈ 9.67, то есть с 10
// рун; тест ниже проверяет и это число, а не только то, что правый край
// защищён.
func TestWriteYGridRightEdgeStaysOutOfPlotArea(t *testing.T) {
	g := chartGeom{w: 400, h: 100, x0: 58, x1: 390, y0: 10, y1: 90}
	s := yScale{top: 0, step: 1}
	const longLabel = "12.3K megabytes" // 15 рун, ширина 90 > x0=58
	var sb strings.Builder
	writeYGrid(&sb, g, s, func(float64) string { return longLabel })

	tags := parseTextTags(t, sb.String())
	if len(tags) != 1 {
		t.Fatalf("ожидалась 1 подпись оси Y, получено %d: %s", len(tags), sb.String())
	}
	tag := tags[0]
	if tag.anchor != "end" {
		t.Fatalf("якорь подписи оси Y = %q, ожидался end", tag.anchor)
	}
	if tag.x > g.x0+0.05 {
		t.Errorf("правый край подписи оси Y = %.2f заходит правее x0=%.1f (в область графика, поверх сетки и данных)", tag.x, g.x0)
	}
	if math.Abs(tag.x-g.x0) > 0.05 {
		t.Errorf("x подписи = %.2f, ожидалось %.2f (прижата ровно к x0 — ширины 90 не хватает и на левый, и на правый край одновременно)", tag.x, g.x0)
	}
	// Записанный компромисс: левый край (x - ширина) в этом сценарии
	// действительно уходит за 0 — обрезается вьюбоксом. Число не «примерно
	// отрицательное», а точное: x0(58) - width(90) = -32.
	width := estimateTextWidth(longLabel)
	if width != 90 {
		t.Fatalf("подставная подпись даёт ширину %.1f, ожидалось 90 — число -32 ниже посчитано под неё", width)
	}
	if leftEdge := tag.x - width; math.Abs(leftEdge-(-32)) > 0.05 {
		t.Errorf("левый край подписи = %.2f, ожидалось -32.00 (записанный компромисс: обрезка слева вместо наложения на график)", leftEdge)
	}
}

// TestWriteYGridLeftClipThreshold — порог, с которого начинается обрезка
// левого края (компромисс из TestWriteYGridRightEdgeStaysOutOfPlotArea):
// ровно x0/svgCharWidthPx рун — подпись ещё помещается, x0/svgCharWidthPx+1
// рун — уже нет. При x0=60 порог — 10 рун ровно.
func TestWriteYGridLeftClipThreshold(t *testing.T) {
	g := chartGeom{w: 400, h: 100, x0: 60, x1: 390, y0: 10, y1: 90}
	s := yScale{top: 0, step: 1}

	fits := "1234567890" // 10 рун, ширина 60 == x0 — левый край ровно 0, не обрезан
	var sbFits strings.Builder
	writeYGrid(&sbFits, g, s, func(float64) string { return fits })
	tagsFits := parseTextTags(t, sbFits.String())
	if len(tagsFits) != 1 {
		t.Fatalf("ожидалась 1 подпись, получено %d", len(tagsFits))
	}
	if leftEdge := tagsFits[0].x - estimateTextWidth(fits); leftEdge < -0.05 {
		t.Errorf("10-рунная подпись при x0=60 не должна обрезаться слева: левый край = %.2f", leftEdge)
	}

	clipped := "12345678901" // 11 рун, ширина 66 > x0=60 — уже обрезается
	var sbClipped strings.Builder
	writeYGrid(&sbClipped, g, s, func(float64) string { return clipped })
	tagsClipped := parseTextTags(t, sbClipped.String())
	if len(tagsClipped) != 1 {
		t.Fatalf("ожидалась 1 подпись, получено %d", len(tagsClipped))
	}
	if leftEdge := tagsClipped[0].x - estimateTextWidth(clipped); math.Abs(leftEdge-(-6)) > 0.05 {
		t.Errorf("11-рунная подпись при x0=60: левый край = %.2f, ожидалось -6.00 (обрезка на 1 руну = 6 единиц)", leftEdge)
	}
}

// TestWriteYGridShortLabelUnclamped — короткая подпись при обычном x0 не
// трогается прижимом: x остаётся g.x0-6, как раньше (регресс не вносим).
func TestWriteYGridShortLabelUnclamped(t *testing.T) {
	g := chartGeom{w: 200, h: 100, x0: 48, x1: 190, y0: 10, y1: 90}
	s := yScale{top: 0, step: 1}
	var sb strings.Builder
	writeYGrid(&sb, g, s, func(float64) string { return "5" })

	tags := parseTextTags(t, sb.String())
	if len(tags) != 1 {
		t.Fatalf("ожидалась 1 подпись оси Y, получено %d: %s", len(tags), sb.String())
	}
	if want := g.x0 - 6; math.Abs(tags[0].x-want) > 0.05 {
		t.Errorf("x короткой подписи = %.2f, ожидалось %.2f (без прижима)", tags[0].x, want)
	}
}

// TestWriteXTicksAccountsForAnchorShift — у левого края холста якорь подписи
// переключается на "start": подпись растёт от x вправо, а не от x-halfWidth,
// то есть её видимый центр сдвигается на полширины вправо. Разводка тиков
// раньше этот сдвиг не учитывала — первая подпись наезжала на вторую
// (P1-7). Сценарий: узкий холст, первый тик ровно на x0 с длинной подписью,
// второй — недалеко от первого с обычной подписью времени.
func TestWriteXTicksAccountsForAnchorShift(t *testing.T) {
	g := chartGeom{w: 220, h: 120, x0: 50, x1: 210, y0: 10, y1: 90}
	ticks := []xTick{
		{x: 50, text: "2026-08-27"}, // 10 рун — заведомо длинная подпись у самого края
		{x: 120, text: "18:00"},     // обычная подпись времени, недалеко от первой
	}
	var sb strings.Builder
	writeXTicks(&sb, g, ticks)

	tags := parseTextTags(t, sb.String())
	if len(tags) != 2 {
		t.Fatalf("ожидались 2 подписи оси X, получено %d: %s", len(tags), sb.String())
	}
	first, second := tags[0], tags[1]
	if first.anchor != "start" {
		t.Fatalf("якорь первой подписи = %q, ожидался start (у левого края холста)", first.anchor)
	}
	// Без учёта сдвига второй якорь остался бы "middle": её левый край был бы
	// x - ширина/2 = 120 - 15 = 105, что меньше правого края первой (50+60=110)
	// — наезд. С учётом сдвига якорь второй тоже переключается на "start".
	if second.anchor != "start" {
		t.Fatalf("якорь второй подписи = %q, ожидался start (иначе наезд на первую — сдвиг не учтён)", second.anchor)
	}

	w0, w1 := estimateTextWidth(ticks[0].text), estimateTextWidth(ticks[1].text)
	_, rightFirst := labelBounds(first.x, first.anchor, w0)
	leftSecond, _ := labelBounds(second.x, second.anchor, w1)
	if gap := leftSecond - rightFirst; gap < 0 {
		t.Errorf("подписи перекрываются: зазор между правым краем первой (%.2f) и левым краем второй (%.2f) = %.2f (%.2f/%s vs %.2f/%s)",
			rightFirst, leftSecond, gap, first.x, first.anchor, second.x, second.anchor)
	}
}

// TestXLabelPlacementEndAnchorChecksPrevRight — защита от наезда была только
// у ветки "start" (см. TestWriteXTicksAccountsForAnchorShift); у ветки "end"
// (тик у правого края холста) её не было вовсе, хотя якорь end сдвигает
// левый край подписи ЕЩЁ левее среднего — риск наезда на предыдущую у него
// выше, а не ниже. Сценарий: тик у самого x1 (якорь неизбежно "end") при
// этом слишком близко к prevRight — сменить якорь на "start" нельзя (это
// увело бы подпись ещё дальше за x1), поэтому единственный целевой исход —
// не рисовать подпись вовсе (draw=false), а не молча накладывать текст.
func TestXLabelPlacementEndAnchorChecksPrevRight(t *testing.T) {
	// text шириной 42 (7 рун × 6px) для круглых чисел.
	const text = "HELLO12"
	if w := estimateTextWidth(text); w != 42 {
		t.Fatalf("подставная подпись даёт ширину %.1f, ожидалось 42 — числа ниже подобраны под неё", w)
	}
	x0, x1 := 0.0, 100.0
	prevRight := 75.0
	x := 88.0 // x+half=109>x1 → якорь неизбежно "end"; x+w=130>x1 → эскалация в "start" невозможна

	anchor, left, right, draw := xLabelPlacement(x0, x1, prevRight, x, text)
	if anchor != "end" {
		t.Fatalf("якорь = %q, ожидался end (тик у правого края холста)", anchor)
	}
	if left != x-42 || right != x {
		t.Fatalf("границы = [%.1f, %.1f], ожидалось [%.1f, %.1f]", left, right, x-42, x)
	}
	if left >= prevRight {
		t.Fatalf("тестовый сценарий сломан: left=%.1f должен быть < prevRight=%.1f, иначе наезда нет и draw=false ничего не проверяет", left, prevRight)
	}
	if draw {
		t.Errorf("draw=true при наезде, который нельзя починить сменой якоря (left=%.1f < prevRight=%.1f, а start увёл бы за x1=%.1f): подпись должна быть подавлена, а не наложена", left, prevRight, x1)
	}
}
