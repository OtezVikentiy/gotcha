package web

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
	require.NoError(t, dependencyMapSVG(context.Background(), deps, 720, 360).Render(context.Background(), &sb))
	out := sb.String()
	require.Contains(t, out, "<svg")
	require.Contains(t, out, "postgresql")
	require.Contains(t, out, "api.stripe.com")

	var sb2 strings.Builder
	require.NoError(t, dependencyMapSVG(context.Background(), deps, 720, 360).Render(context.Background(), &sb2))
	require.Equal(t, out, sb2.String())
}
