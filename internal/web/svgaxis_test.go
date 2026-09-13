package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

// ширина холста, при которой svgCharWidthPx даёт ≈18.0 на руну — сценарии
// ниже посчитаны под эту ширину; точность держит TestCalWCharWidth.
const calW = 629

func TestCalWCharWidth(t *testing.T) {
	if cw := svgCharWidthPx(calW); math.Abs(cw-18) > 0.01 {
		t.Fatalf("svgCharWidthPx(%d) = %.4f, тесты ниже рассчитаны на ≈18.0", calW, cw)
	}
}

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

func TestYScaleHeadroom(t *testing.T) {
	if s := newYScale(10, 3); s.top <= 10 {
		t.Errorf("newYScale(10): top = %v, ожидался запас над максимумом", s.top)
	}
	if s := newYScaleFloat(90_000, 3); s.top <= 90_000 {
		t.Errorf("newYScaleFloat(90000): top = %v, ожидался запас над максимумом", s.top)
	}
}

// сценарий: ширины подписи хватает и на прижим к x=0, и на то, чтобы
// остаться левее x0 — проверяются оба края.
func TestWriteYGridClampsLongLabelToCanvas(t *testing.T) {
	g := chartGeom{w: calW, h: 100, x0: 288, x1: 478, y0: 10, y1: 90}
	s := yScale{top: 0, step: 1}         // один тик, v=0 — геометрия подписи не зависит от значения
	const longLabel = "1234567890123456" // 16 рун ≈ 288 — шире x0-6=282, не шире x0=288
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
	width := estimateTextWidth(g.w, longLabel)
	leftEdge := tag.x - width
	if leftEdge < -0.05 {
		t.Errorf("левый край подписи оси Y = %.2f (x=%.2f, ширина=%.2f) — уходит за x=0", leftEdge, tag.x, width)
	}
	if tag.x > g.x0+0.05 {
		t.Errorf("правый край подписи оси Y = %.2f заходит правее x0=%.1f — залезает в область графика", tag.x, g.x0)
	}
	// без прижима x=282, левый край отрицателен; с прижимом x=width≈288,
	// левый край=0, и это не превышает x0=288.
	if math.Abs(tag.x-width) > 0.05 {
		t.Errorf("x подписи = %.2f, ожидалось %.2f (x0-6 недостаточно для этой подписи, прижато так, что левый край = 0)", tag.x, width)
	}
}

// x0=58, подпись шириной ≈270 (unit без ограничения длины) — прижатый левый
// край оказался бы правее x0; тест проверяет клампинг к x0.
func TestWriteYGridRightEdgeStaysOutOfPlotArea(t *testing.T) {
	g := chartGeom{w: calW, h: 100, x0: 58, x1: 390, y0: 10, y1: 90}
	s := yScale{top: 0, step: 1}
	const longLabel = "12.3K megabytes" // 15 рун, ширина ≈270 > x0=58
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
		t.Errorf("x подписи = %.2f, ожидалось %.2f (прижата ровно к x0 — ширины 270 не хватает и на левый, и на правый край одновременно)", tag.x, g.x0)
	}
	// компромисс: левый край всё равно уходит за 0 (обрезается вьюбоксом) —
	// точное значение ≈-212, не «примерно отрицательное».
	width := estimateTextWidth(g.w, longLabel)
	if math.Abs(width-270) > 0.05 {
		t.Fatalf("подставная подпись даёт ширину %.2f, ожидалось ≈270 — число -212 ниже посчитано под неё", width)
	}
	if leftEdge := tag.x - width; math.Abs(leftEdge-(g.x0-width)) > 0.05 || leftEdge > -211.9 {
		t.Errorf("левый край подписи = %.2f, ожидалось ≈-212 (записанный компромисс: обрезка слева вместо наложения на график)", leftEdge)
	}
}

// порог обрезки левого края — ровно x0/svgCharWidthPx рун; при x0=180 это 10 рун.
func TestWriteYGridLeftClipThreshold(t *testing.T) {
	g := chartGeom{w: calW, h: 100, x0: 180, x1: 390, y0: 10, y1: 90}
	s := yScale{top: 0, step: 1}

	fits := "1234567890" // 10 рун, ширина ≈180 == x0 — левый край ровно 0, не обрезан
	var sbFits strings.Builder
	writeYGrid(&sbFits, g, s, func(float64) string { return fits })
	tagsFits := parseTextTags(t, sbFits.String())
	if len(tagsFits) != 1 {
		t.Fatalf("ожидалась 1 подпись, получено %d", len(tagsFits))
	}
	if leftEdge := tagsFits[0].x - estimateTextWidth(g.w, fits); leftEdge < -0.05 {
		t.Errorf("10-рунная подпись при x0=180 не должна обрезаться слева: левый край = %.2f", leftEdge)
	}

	clipped := "12345678901" // 11 рун, ширина ≈198 > x0=180 — уже обрезается
	var sbClipped strings.Builder
	writeYGrid(&sbClipped, g, s, func(float64) string { return clipped })
	tagsClipped := parseTextTags(t, sbClipped.String())
	if len(tagsClipped) != 1 {
		t.Fatalf("ожидалась 1 подпись, получено %d", len(tagsClipped))
	}
	if leftEdge := tagsClipped[0].x - estimateTextWidth(g.w, clipped); math.Abs(leftEdge-(-18)) > 0.05 {
		t.Errorf("11-рунная подпись при x0=180: левый край = %.2f, ожидалось -18.00 (обрезка на 1 руну = 18 единиц)", leftEdge)
	}
}

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

// у левого края якорь start растёт вправо от x — видимый центр подписи
// сдвигается на полширины; разводка тиков обязана это учитывать.
func TestWriteXTicksAccountsForAnchorShift(t *testing.T) {
	g := chartGeom{w: calW, h: 120, x0: 50, x1: 468, y0: 10, y1: 90}
	ticks := []xTick{
		{x: 50, text: "2026-08-27"}, // 10 рун — заведомо длинная подпись у самого края
		{x: 250, text: "18:00"},     // за правым краем первой подписи
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
	// без учёта сдвига якорь второй остался бы middle (левый край ≈205 <
	// правого края первой ≈230 — наезд); с учётом сдвига оба start.
	if second.anchor != "start" {
		t.Fatalf("якорь второй подписи = %q, ожидался start (иначе наезд на первую — сдвиг не учтён)", second.anchor)
	}

	w0, w1 := estimateTextWidth(g.w, ticks[0].text), estimateTextWidth(g.w, ticks[1].text)
	_, rightFirst := labelBounds(first.x, first.anchor, w0)
	leftSecond, _ := labelBounds(second.x, second.anchor, w1)
	if gap := leftSecond - rightFirst; gap < 0 {
		t.Errorf("подписи перекрываются: зазор между правым краем первой (%.2f) и левым краем второй (%.2f) = %.2f (%.2f/%s vs %.2f/%s)",
			rightFirst, leftSecond, gap, first.x, first.anchor, second.x, second.anchor)
	}
}

// защита от наезда раньше была только у ветки start — у end её не было,
// хотя end сдвигает левый край подписи ещё левее (риск наезда выше).
func TestXLabelPlacementEndAnchorChecksPrevRight(t *testing.T) {
	// text шириной ≈126 (7 рун × ≈18 при calW) для круглых чисел.
	const text = "HELLO12"
	w := estimateTextWidth(calW, text)
	if math.Abs(w-126) > 0.05 {
		t.Fatalf("подставная подпись даёт ширину %.2f, ожидалось ≈126 — числа ниже подобраны под неё", w)
	}
	x0, x1 := 0.0, 300.0
	prevRight := 225.0
	x := 264.0 // x+half=327>x1 → якорь неизбежно "end"; x+w=390>x1 → эскалация в "start" невозможна

	anchor, left, right, draw := xLabelPlacement(calW, x0, x1, prevRight, x, text)
	if anchor != "end" {
		t.Fatalf("якорь = %q, ожидался end (тик у правого края холста)", anchor)
	}
	if math.Abs(left-(x-w)) > 1e-9 || right != x {
		t.Fatalf("границы = [%.1f, %.1f], ожидалось [%.1f, %.1f]", left, right, x-w, x)
	}
	if left >= prevRight {
		t.Fatalf("тестовый сценарий сломан: left=%.1f должен быть < prevRight=%.1f, иначе наезда нет и draw=false ничего не проверяет", left, prevRight)
	}
	if draw {
		t.Errorf("draw=true при наезде, который нельзя починить сменой якоря (left=%.1f < prevRight=%.1f, а start увёл бы за x1=%.1f): подпись должна быть подавлена, а не наложена", left, prevRight, x1)
	}
}
