package web

import (
	"math"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
)

// padL=64 не вмещал «400ms» (≈75 единиц) — fitYLabels должен раздвинуть
// поле под самую широкую подпись + yLabelGap.
func TestFitYLabelsWidensPadForTierWidth(t *testing.T) {
	g := newChartGeom(perfLatencyChartWidth, perfLatencyChartHeight, 64, 16, 26, 26)
	s := newYScaleFloat(400_000, 3) // мкс → «200ms», «400ms», …
	widest := 0.0
	for v := 0.0; v <= s.top+s.step/2; v += s.step {
		if w := g.textWidth(formatUSAxis(v)); w > widest {
			widest = w
		}
	}
	if widest <= g.x0 {
		t.Fatalf("сценарий сломан: самая широкая подпись %.1f должна быть шире padL=%.0f", widest, g.x0)
	}

	fit := g.fitYLabels(s, formatUSAxis)
	if want := widest + yLabelGap; math.Abs(fit.x0-want) > 0.05 {
		t.Fatalf("fitYLabels: x0 = %.2f, ожидалось %.2f (самая широкая подпись + зазор)", fit.x0, want)
	}
	if fit.x1 != g.x1 || fit.y0 != g.y0 || fit.y1 != g.y1 {
		t.Fatalf("fitYLabels меняет не только x0: %+v vs %+v", fit, g)
	}

	var sb strings.Builder
	writeYGrid(&sb, fit, s, formatUSAxis)
	for _, tag := range parseTextTags(t, sb.String()) {
		if tag.anchor != "end" {
			t.Fatalf("якорь подписи оси Y = %q, ожидался end", tag.anchor)
		}
		// проверяем по самой широкой подписи — прижим writeYGrid именно у неё.
		if left := tag.x - widest; tag.x > fit.x0-yLabelGap+0.05 && left < -0.05 {
			t.Errorf("подпись оси Y на x=%.2f выходит за левый край вьюбокса (левый край %.2f)", tag.x, left)
		}
	}
}

func TestFitYLabelsKeepsPadForShortLabels(t *testing.T) {
	g := newChartGeom(latencyChartWidth, latencyChartHeight, 48, 16, 26, 26)
	s := newYScale(10, 3)
	fit := g.fitYLabels(s, formatCountAxis)
	if fit != g {
		t.Fatalf("короткие подписи не должны менять геометрию: %+v vs %+v", fit, g)
	}
}

func TestFitYLabelsZeroStepIsNoop(t *testing.T) {
	g := newChartGeom(720, 200, 48, 16, 26, 26)
	fit := g.fitYLabels(yScale{top: 0, step: 0}, formatCountAxis)
	if fit != g {
		t.Fatalf("нулевой шаг: геометрия должна остаться прежней: %+v vs %+v", fit, g)
	}
}

// патологически длинный unit не съедает график — поле растёт не выше
// yLabelPadMaxShare, дальше работает обрезка слева.
func TestYAxisPadLCapsAtQuarterOfCanvas(t *testing.T) {
	long := strings.Repeat("x", 40)
	got := yAxisPadL(1200, 58, []string{"0", long})
	if want := 1200 * yLabelPadMaxShare; got != want {
		t.Fatalf("yAxisPadL = %.1f, ожидалось %.1f (предел четверти холста)", got, want)
	}
	if got := yAxisPadL(1200, 58, []string{"0", "5"}); got != 58 {
		t.Fatalf("короткие подписи: yAxisPadL = %.1f, ожидалось padL=58", got)
	}
}

// chartBars ведёт свою шкалу, но поле берёт из общей yAxisPadL — широкие
// счётчики должны сдвигать ось правее.
func TestChartBarsWidensPadForWideCounts(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	points := []event.Point{{T: base, N: 1500}, {T: base.Add(time.Hour), N: 2000}}
	out := chartBars(t.Context(), points, chartWidth, chartHeight)
	tags := parseTextTags(t, out)
	if len(tags) == 0 {
		t.Fatalf("нет подписей: %s", out)
	}
	widest := estimateTextWidth(chartWidth, "3000")
	for _, tag := range tags {
		if tag.anchor != "end" {
			continue // подписи дней
		}
		if left := tag.x - widest; left < -0.05 {
			t.Errorf("подпись счётчика на x=%.2f обрезана слева (левый край %.2f): поле не раздвинуто", tag.x, left)
		}
		if tag.x <= float64(chartPadL)-yLabelGap+0.05 {
			t.Errorf("подпись счётчика на x=%.2f стоит на старом поле chartPadL=%d — yAxisPadL не применён", tag.x, chartPadL)
		}
	}
}

func deployGeom() chartGeom {
	return newChartGeom(perfLatencyChartWidth, perfLatencyChartHeight, 64, 16, 26, 26)
}

// зазор между подписями версий — по ширине подписи на тире, не константой.
// деплои подаются в порядке БД (DESC): без сортировки «предыдущей» была бы самая правая.
func TestWriteDeployMarkerLabelGapFollowsTierWidth(t *testing.T) {
	g := deployGeom()
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(3 * time.Hour)}
	span := g.x1 - g.x0 // единиц на 3 часа
	at := func(units float64) time.Time {
		return base.Add(time.Duration(units / span * 3 * float64(time.Hour)))
	}
	w := g.textWidth("v1.2.2")
	if w < 60 {
		t.Fatalf("сценарий сломан: ширина подписи %.1f должна быть заметно больше прежнего зазора 44", w)
	}
	// смещения считаются от ширины подписи, не константой — иначе тест
	// рассинхронизируется с калибровкой svgCharWidthPerVB при следующей правке.
	suppressedAt := w / 2
	drawnAt := w + 20
	deploys := []deploy.Deployment{
		{Version: "v1.2.4", DeployedAt: at(drawnAt)},
		{Version: "v1.2.3", DeployedAt: at(suppressedAt)},
		{Version: "v1.2.2", DeployedAt: at(0)},
	}
	var sb strings.Builder
	writeDeployMarker(&sb, g, times, deploys)
	out := sb.String()

	if got := strings.Count(out, "chart-deploy-marker"); got != 3 {
		t.Errorf("линии маркеров рисуются все: ожидалось 3, got %d: %s", got, out)
	}
	label := func(v string) bool { return strings.Contains(out, `">`+v+`</text>`) }
	if !label("v1.2.2") {
		t.Errorf("первая подпись должна рисоваться: %s", out)
	}
	if label("v1.2.3") {
		t.Errorf("вторая подпись в %.1f единицах от первой (ширина %.1f) должна быть подавлена: %s", suppressedAt, w, out)
	}
	if !label("v1.2.4") {
		t.Errorf("третья подпись в %.1f единицах от нарисованной (ширина %.1f) должна рисоваться: %s", drawnAt, w, out)
	}
}

// разворот подписи у правого края — тоже по ширине подписи, не фиксированным
// порогом, иначе подпись вылезает за холст.
func TestWriteDeployMarkerEndAnchorByLabelWidth(t *testing.T) {
	g := deployGeom()
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(3 * time.Hour)}
	span := g.x1 - g.x0
	x := g.x1 - 62
	deploys := []deploy.Deployment{{
		Version:    "v1.2.2",
		DeployedAt: base.Add(time.Duration((x - g.x0) / span * 3 * float64(time.Hour))),
	}}
	var sb strings.Builder
	writeDeployMarker(&sb, g, times, deploys)
	out := sb.String()
	if !strings.Contains(out, `text-anchor="end"`) {
		t.Fatalf("подпись у правого края шире оставшегося места — ожидался якорь end: %s", out)
	}
	tags := parseTextTags(t, out)
	if len(tags) != 1 {
		t.Fatalf("ожидалась 1 подпись, получено %d: %s", len(tags), out)
	}
	if tags[0].x > g.x1 {
		t.Errorf("правый край подписи %.2f правее x1=%.2f", tags[0].x, g.x1)
	}
}
