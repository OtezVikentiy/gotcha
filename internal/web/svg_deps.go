package web

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
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
// depsMapNodeCap — карта показывает только топ-N зависимостей (deps уже
// отсортированы по числу вызовов). Радиальная раскладка на десятки узлов
// нечитаема (окружность не резиновая), поэтому лишние остаются только в
// таблице ниже — под картой рисуется пометка «+N ещё». Полный список (до
// depsLimit=50) всегда в таблице.
const depsMapNodeCap = 8

func dependencyMapSVG(ctx context.Context, deps []templates.DependencyRow, w, h int) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, wr io.Writer) error {
		var sb strings.Builder
		sb.WriteString(svgRoot("deps-map", w, h, i18n.T(ctx, "region.deps.map")))
		cx, cy := float64(w)/2, float64(h)/2
		// radius: центр отстоит от узла минимум на полуширину узла (50) + зазор,
		// иначе узел на угле 0° налезает на хаб (была причина перекрытия).
		radius := math.Min(cx, cy) - 60
		sb.WriteString(depNodeSVG(cx, cy, i18n.T(ctx, "deps.center"), "deps-node deps-center"))
		shown := deps
		if len(shown) > depsMapNodeCap {
			shown = shown[:depsMapNodeCap]
		}
		n := len(shown)
		for i, d := range shown {
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
			sb.WriteString(fmt.Sprintf(`<text class="deps-edge-label" x="%s" y="%s" text-anchor="%s">%s</text>`,
				formatCoord(mx), formatCoord(my), depEdgeLabelAnchor(x-cx), templ.EscapeString(label)))
			sb.WriteString(depNodeSVG(x, y, d.Target, "deps-node"))
		}
		if len(deps) > depsMapNodeCap {
			more := i18n.Tf(ctx, "deps.map_more", "n", strconv.Itoa(len(deps)-depsMapNodeCap))
			sb.WriteString(fmt.Sprintf(`<text class="deps-more" x="%s" y="%s" text-anchor="middle">%s</text>`,
				formatCoord(cx), formatCoord(float64(h)-12), templ.EscapeString(more)))
		}
		sb.WriteString(`</svg>`)
		_, err := io.WriteString(wr, sb.String())
		return err
	})
}

// depEdgeLabelAnchor выбирает якорь подписи ребра так, чтобы текст рос ОТ
// узла, а не под него: без text-anchor подпись росла слева направо от точки
// привязки, а следующей строкой рисуется прямоугольник узла — на самой
// нагруженной зависимости (индекс 0, угол 0°, узел строго справа от хаба)
// подпись гарантированно уезжала под него. Узел лежит дальше от хаба, чем
// середина ребра, в направлении dx = x-cx, поэтому подпись должна расти в
// противоположную сторону: dx>0 (узел справа) → "end" (текст растёт влево),
// dx<0 (узел слева) → "start" (текст растёт вправо). Знак dx определён для
// любого угла ребра, но само отсутствие наложения на прямоугольник узла —
// нет: оно держится на текущих constants этого файла (radius = min(cx,cy)-60,
// узел 100×28 в depNodeSVG) и на том, что подпись обычно короче зазора между
// серединой ребра и узлом. На меньшем радиусе, более крупном узле или
// длинной подписи (Target длиннее обычного) якорь по-прежнему уводит текст в
// верном направлении, но расстояния может не хватить, и наложение вернётся.
func depEdgeLabelAnchor(dx float64) string {
	switch {
	case dx > 0:
		return "end"
	case dx < 0:
		return "start"
	default:
		return "middle"
	}
}

// depNodeSVG рисует один узел карты — подписанный прямоугольник с центром в
// (cx, cy). class различает узел-владельца ("deps-node deps-center") от
// узлов зависимостей ("deps-node").
func depNodeSVG(cx, cy float64, text, class string) string {
	return fmt.Sprintf(`<g class="%s"><rect x="%s" y="%s" rx="6" width="100" height="28"/><text x="%s" y="%s" text-anchor="middle">%s</text></g>`,
		class, formatCoord(cx-50), formatCoord(cy-14), formatCoord(cx), formatCoord(cy+4), templ.EscapeString(text))
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
