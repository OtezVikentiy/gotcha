package web

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// dependencyMapSVG рисует hub-and-spoke карту: сервис-владелец в центре,
// зависимости (БД/кеш/HTTP) радиально вокруг него, ребро подписано p95 и
// долей ошибок. Раскладка детерминирована: порядок узлов = порядок deps
// (уже отсортирован по числу вызовов в dependencies.go), угол i-го узла —
// i/N·2π — без учёта текущего времени или недетерминированной итерации
// map, поэтому два рендера одних и тех же данных дают идентичный вывод (см.
// TestDependencyMapSVG).
func dependencyMapSVG(ctx context.Context, deps []templates.DependencyRow, w, h int) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, wr io.Writer) error {
		var sb strings.Builder
		sb.WriteString(svgRoot("deps-map", w, h, i18n.T(ctx, "region.deps.map")))
		cx, cy := float64(w)/2, float64(h)/2
		radius := math.Min(cx, cy) - 90
		sb.WriteString(depNodeSVG(cx, cy, i18n.T(ctx, "deps.center"), "deps-node deps-center"))
		n := len(deps)
		for i, d := range deps {
			ang := (float64(i) / float64(max(n, 1))) * 2 * math.Pi
			x := cx + radius*math.Cos(ang)
			y := cy + radius*math.Sin(ang)
			edgeClass := "deps-edge"
			if d.ErrorRate >= 0.05 {
				edgeClass = "deps-edge deps-edge-bad"
			} else if d.ErrorRate > 0 {
				edgeClass = "deps-edge deps-edge-warn"
			}
			sb.WriteString(fmt.Sprintf(`<line class="%s" x1="%s" y1="%s" x2="%s" y2="%s"/>`,
				edgeClass, formatCoord(cx), formatCoord(cy), formatCoord(x), formatCoord(y)))
			mx, my := (cx+x)/2, (cy+y)/2
			label := fmt.Sprintf("p95 %s · %s", depMicros(d.P95US), depPercent(d.ErrorRate))
			sb.WriteString(fmt.Sprintf(`<text class="deps-edge-label" x="%s" y="%s">%s</text>`,
				formatCoord(mx), formatCoord(my), templ.EscapeString(label)))
			sb.WriteString(depNodeSVG(x, y, d.Target, "deps-node"))
		}
		sb.WriteString(`</svg>`)
		_, err := io.WriteString(wr, sb.String())
		return err
	})
}

// depNodeSVG рисует один узел карты — подписанный прямоугольник с центром в
// (cx, cy). class различает узел-владельца ("deps-node deps-center") от
// узлов зависимостей ("deps-node").
func depNodeSVG(cx, cy float64, text, class string) string {
	return fmt.Sprintf(`<g class="%s"><rect x="%s" y="%s" rx="6" width="120" height="30"/><text x="%s" y="%s" text-anchor="middle">%s</text></g>`,
		class, formatCoord(cx-60), formatCoord(cy-15), formatCoord(cx), formatCoord(cy+5), templ.EscapeString(text))
}

// depMicros форматирует микросекунды для подписи ребра: локальный форматтер
// (не formatDurationUS из package templates — та недостижима отсюда, package
// web не может звать package templates вспомогательные функции экрана).
// Пороги как в остальных местах продукта: <1мс — микросекунды, <1с —
// миллисекунды, иначе секунды с одним знаком после запятой.
func depMicros(us uint32) string {
	switch {
	case us < 1000:
		return fmt.Sprintf("%dµs", us)
	case us < 1_000_000:
		return fmt.Sprintf("%dms", us/1000)
	default:
		return fmt.Sprintf("%.1fs", float64(us)/1_000_000)
	}
}

// depPercent форматирует долю ошибок для подписи ребра ("N.N%") — локальный
// аналог formatFailureRate (package templates, недостижим из package web).
func depPercent(rate float64) string {
	return fmt.Sprintf("%.1f%%", rate*100)
}
