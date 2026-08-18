package web

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// TestDependencyMapSVG — hub-SVG карты зависимостей: сервис в центре, узлы
// зависимостей радиально, рёбра с подписью p95/error. Проверяем содержимое
// (имена целей присутствуют) и детерминизм (два рендера одних и тех же
// данных дают идентичный вывод — раскладка не зависит от map-итерации или
// времени).
func TestDependencyMapSVG(t *testing.T) {
	deps := []templates.DependencyRow{
		{Kind: "database", Target: "postgresql", Calls: 1200, P50US: 3000, P95US: 8000, ErrorRate: 0.001},
		{Kind: "http", Target: "api.stripe.com", Calls: 40, P50US: 60000, P95US: 120000, ErrorRate: 0.02},
	}
	var sb strings.Builder
	if err := dependencyMapSVG(context.Background(), deps, 720, 360).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	for _, want := range []string{"<svg", "postgresql", "api.stripe.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("SVG не содержит %q: %s", want, out)
		}
	}

	var sb2 strings.Builder
	if err := dependencyMapSVG(context.Background(), deps, 720, 360).Render(context.Background(), &sb2); err != nil {
		t.Fatalf("render 2: %v", err)
	}
	if out != sb2.String() {
		t.Errorf("рендер недетерминирован: два прогона разошлись")
	}
}
