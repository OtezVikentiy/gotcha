package web

import (
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
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
