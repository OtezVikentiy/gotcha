package web

import (
	"context"
	"fmt"
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

// TestDependencyMapSVGCap — карта кэпируется топ-N узлами (depsMapNodeCap),
// лишние остаются только в таблице; под картой — пометка «+N ещё» (аудит UX
// P1: радиальная раскладка на десятки узлов нечитаема).
func TestDependencyMapSVGCap(t *testing.T) {
	var deps []templates.DependencyRow
	for i := 0; i < depsMapNodeCap+4; i++ {
		deps = append(deps, templates.DependencyRow{Kind: "http", Target: fmt.Sprintf("svc-%02d", i), Calls: int64(100 - i)})
	}
	var sb strings.Builder
	if err := dependencyMapSVG(context.Background(), deps, 720, 420).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := sb.String()
	// последний узел в пределах кэпа нарисован
	if !strings.Contains(out, fmt.Sprintf("svc-%02d", depsMapNodeCap-1)) {
		t.Errorf("узел в пределах кэпа не нарисован: %s", out)
	}
	// первый узел за кэпом — НЕ нарисован
	if strings.Contains(out, fmt.Sprintf("svc-%02d", depsMapNodeCap)) {
		t.Errorf("узел за кэпом (svc-%02d) не должен рисоваться на карте", depsMapNodeCap)
	}
	// пометка про остаток
	if !strings.Contains(out, "deps-more") {
		t.Errorf("нет пометки «+N ещё» при превышении кэпа")
	}
}
