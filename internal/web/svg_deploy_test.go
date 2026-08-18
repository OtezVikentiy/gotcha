package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// renderDeployMarkerForTest собирает реальный chartGeom (как markup-функции) и
// прогоняет writeDeployMarker в буфер — возвращает получившийся SVG-фрагмент.
func renderDeployMarkerForTest(times []time.Time, deploys []deploy.Deployment) string {
	g := newChartGeom(600, 200, 58, 16, 12, 26)
	var sb strings.Builder
	writeDeployMarker(&sb, g, times, deploys)
	return sb.String()
}

func TestWriteDeployMarker(t *testing.T) {
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)}

	// Деплой в середине окна: рисуется линия маркера + подпись версии.
	out := renderDeployMarkerForTest(times, []deploy.Deployment{{Version: "v9", DeployedAt: base.Add(time.Hour)}})
	if !strings.Contains(out, "chart-deploy-marker") {
		t.Errorf("нет линии маркера: %s", out)
	}
	if !strings.Contains(out, "chart-deploy-label") || !strings.Contains(out, "v9") {
		t.Errorf("нет подписи версии: %s", out)
	}

	// Деплой ВНЕ диапазона точек не рисуется.
	out2 := renderDeployMarkerForTest(times, []deploy.Deployment{{Version: "old", DeployedAt: base.Add(-time.Hour)}})
	if strings.Contains(out2, "old") || strings.Contains(out2, "chart-deploy-marker") {
		t.Errorf("деплой вне окна не должен рисоваться: %s", out2)
	}

	// Версия экранируется (никакого сырого <script> в разметке).
	out3 := renderDeployMarkerForTest(times, []deploy.Deployment{{Version: `<script>`, DeployedAt: base.Add(time.Hour)}})
	if strings.Contains(out3, "<script>") {
		t.Errorf("версия не экранирована: %s", out3)
	}
	if !strings.Contains(out3, "&lt;script&gt;") {
		t.Errorf("ожидалась экранированная версия: %s", out3)
	}

	// Пустой список деплоев и одна точка — ничего не рисуется (без паники).
	if got := renderDeployMarkerForTest(times, nil); got != "" {
		t.Errorf("пустой список деплоев должен дать пусто: %q", got)
	}
	if got := renderDeployMarkerForTest(times[:1], []deploy.Deployment{{Version: "v1", DeployedAt: base}}); got != "" {
		t.Errorf("одна точка — маркер не рисуется: %q", got)
	}
}

// TestWriteDeployMarkerTail — деплой в хвосте (позже times[last], т.е. между
// последней корзиной и now) обязан рисоваться у правого края графика (x1), а не
// отбрасываться: это ядро фичи «только что выкатил». Подпись у края
// разворачивается на text-anchor="end", иначе версия вылезла бы за холст.
func TestWriteDeployMarkerTail(t *testing.T) {
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)}

	// geom как в renderDeployMarkerForTest: x1 = 600-16 = 584.
	out := renderDeployMarkerForTest(times, []deploy.Deployment{
		{Version: "vNew", DeployedAt: base.Add(3 * time.Hour)}, // час ПОСЛЕ последней точки
	})
	if !strings.Contains(out, "chart-deploy-marker") || !strings.Contains(out, "vNew") {
		t.Fatalf("хвостовой деплой должен рисоваться: %s", out)
	}
	if !strings.Contains(out, `x1="584.0"`) {
		t.Errorf("хвостовой маркер должен быть прижат к правому краю x1=584: %s", out)
	}
	if !strings.Contains(out, `text-anchor="end"`) {
		t.Errorf("подпись у правого края должна иметь якорь end: %s", out)
	}
	// <title> маркера несёт версию и человекочитаемое время (разделитель «·»).
	if !strings.Contains(out, "<title>") || !strings.Contains(out, "·") {
		t.Errorf("нет title с временем на линии маркера: %s", out)
	}
}

// TestWriteDeployMarkerMidAnchor — деплой в середине окна подписывается с
// якорем start (текст вправо от линии), не end: край переопределяет только у x1.
func TestWriteDeployMarkerMidAnchor(t *testing.T) {
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)}
	out := renderDeployMarkerForTest(times, []deploy.Deployment{{Version: "vMid", DeployedAt: base.Add(time.Hour)}})
	if !strings.Contains(out, `text-anchor="start"`) {
		t.Errorf("подпись в середине окна должна иметь якорь start: %s", out)
	}
}

// TestWriteDeployMarkerLabelAntiCollision — при кучных деплоях ЛИНИИ рисуются
// все (маркер важнее подписи), а ПОДПИСИ прореживаются: вторая версия ближе
// minGap к первой нарисованной не печатается, иначе они наехали бы в кашу.
func TestWriteDeployMarkerLabelAntiCollision(t *testing.T) {
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)}
	// Два деплоя в 2 минутах друг от друга: по ширине графика это ~9px < 44px.
	out := renderDeployMarkerForTest(times, []deploy.Deployment{
		{Version: "vA", DeployedAt: base.Add(time.Hour)},
		{Version: "vB", DeployedAt: base.Add(time.Hour + 2*time.Minute)},
	})
	if got := strings.Count(out, "chart-deploy-marker"); got != 2 {
		t.Errorf("обе линии маркеров должны рисоваться, got %d: %s", got, out)
	}
	if got := strings.Count(out, "chart-deploy-label"); got != 1 {
		t.Errorf("кучные подписи должны прорежаться до одной, got %d: %s", got, out)
	}
}

// TestThroughputBarsDeployMarker — на СТОЛБЧАТОМ графике (в отличие от линейной
// геометрии svg_deploy выше) маркер деплоя тоже ложится в шкалу времени слотов.
func TestThroughputBarsDeployMarker(t *testing.T) {
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	points := []trace.LatencyPoint{
		{T: base, Count: 5},
		{T: base.Add(time.Hour), Count: 8},
		{T: base.Add(2 * time.Hour), Count: 3},
	}
	deploys := []deploy.Deployment{{Version: "vBar", DeployedAt: base.Add(time.Hour)}}
	out := throughputBarsMarkup(context.Background(), points, deploys, 600, 200)
	if !strings.Contains(out, "chart-deploy-marker") || !strings.Contains(out, "vBar") {
		t.Errorf("маркер деплоя должен рисоваться на столбчатом графике: %s", out)
	}
}
