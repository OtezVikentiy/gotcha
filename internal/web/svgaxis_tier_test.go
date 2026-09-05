package web

import (
	"math"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
)

// TestFitYLabelsWidensPadForTierWidth — геометрия latencyLinesMarkup
// (chart-vb1200, padL=64): подпись «400ms» по калибровке тира ≥700px занимает
// 5×15.06≈75 единиц и в поле 64 не помещалась — writeYGrid прижимал её к x0,
// и левый край уходил за вьюбокс (аудит 09-04 K9-4: «l00ms» на 1000-1300px).
// fitYLabels должен раздвинуть поле под самую широкую подпись + yLabelGap,
// после чего ни одна подпись writeYGrid не выходит за x=0.
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
		// Ширина подписи по позиции: все подписи тут не короче «0», проверяем
		// по самой широкой — прижим writeYGrid у неё же.
		if left := tag.x - widest; tag.x > fit.x0-yLabelGap+0.05 && left < -0.05 {
			t.Errorf("подпись оси Y на x=%.2f выходит за левый край вьюбокса (левый край %.2f)", tag.x, left)
		}
	}
}

// TestFitYLabelsKeepsPadForShortLabels — короткие подписи («0», «5», «10»)
// поле не трогают: x0 остаётся padL, как задал вызывающий.
func TestFitYLabelsKeepsPadForShortLabels(t *testing.T) {
	g := newChartGeom(latencyChartWidth, latencyChartHeight, 48, 16, 26, 26)
	s := newYScale(10, 3)
	fit := g.fitYLabels(s, formatCountAxis)
	if fit != g {
		t.Fatalf("короткие подписи не должны менять геометрию: %+v vs %+v", fit, g)
	}
}

// TestFitYLabelsZeroStepIsNoop — шкала с нулевым шагом (пустая) не должна
// зацикливать перебор уровней.
func TestFitYLabelsZeroStepIsNoop(t *testing.T) {
	g := newChartGeom(720, 200, 48, 16, 26, 26)
	fit := g.fitYLabels(yScale{top: 0, step: 0}, formatCountAxis)
	if fit != g {
		t.Fatalf("нулевой шаг: геометрия должна остаться прежней: %+v vs %+v", fit, g)
	}
}

// TestYAxisPadLCapsAtQuarterOfCanvas — патологически длинный unit (OTLP) не
// съедает график: поле растёт не выше yLabelPadMaxShare холста, дальше
// работает компромисс writeYGrid (обрезка слева).
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

// TestChartBarsWidensPadForWideCounts — chartBars (частота на issue) ведёт
// шкалу сам, но поле под подписи берёт из той же yAxisPadL: счётчики
// «1000»+ на chart-vb1200 шире chartPadL=40, и ось должна встать правее.
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

// deployGeom — геометрия latencyLinesMarkup (chart-vb1200), на которой аудит
// снял слипшиеся подписи версий.
func deployGeom() chartGeom {
	return newChartGeom(perfLatencyChartWidth, perfLatencyChartHeight, 64, 16, 26, 26)
}

// TestWriteDeployMarkerLabelGapFollowsTierWidth — зазор между подписями
// версий считается по ширине подписи на тире, а не константой 44: на
// chart-vb1200 «v1.2.2» ≈90 единиц, два деплоя в 62 единицах друг от друга
// раньше подписывались оба (62 ≥ 44) и накладывались («v1.2.2v1.2.3»), теперь
// вторая подпись подавляется, третья (в 106 от второй, 168 от первой)
// рисуется. Деплои подаются в порядке БД (DESC): без сортировки по времени
// «предыдущей нарисованной» оказалась бы самая правая, и подписи слева от
// неё подавлялись бы все.
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
	deploys := []deploy.Deployment{
		{Version: "v1.2.4", DeployedAt: at(168)},
		{Version: "v1.2.3", DeployedAt: at(62)},
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
		t.Errorf("вторая подпись в 62 единицах от первой (ширина %.1f) должна быть подавлена: %s", w, out)
	}
	if !label("v1.2.4") {
		t.Errorf("третья подпись в 106 единицах от подавленной и 168 от нарисованной должна рисоваться: %s", out)
	}
}

// TestWriteDeployMarkerEndAnchorByLabelWidth — разворот подписи у правого
// края тоже считается по ширине подписи: маркер в 62 единицах от x1 при
// подписи ≈90 раньше оставался start (порог был 40 единиц) и вылезал за
// холст, теперь — end, и правый край подписи не правее x1.
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
