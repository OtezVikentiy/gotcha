package web

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// константы фиксированы в единицах viewBox — тесты опираются на них напрямую.
const (
	depsMapWidth  = 760
	depsMapPitch  = 60  // шаг строки в колонке
	depsMapNodeW  = 220 // ширина узла
	depsMapNodeH  = 44  // высота узла
	depsMapNodeDY = 8   // отступ верха узла от начала строки
	depsMapLeftX  = 24  // левая колонка: x узла (правый край 244)
	depsMapRightX = 516 // правая колонка: x узла (правый край 736)
	depsMapHubW   = 140
	depsMapHubH   = 48
	depsMapMinH   = 120 // минимальная высота: одна строка и хаб
	depsMapPadH   = 32  // воздух сверху и снизу колонок
	depsMapMoreH  = 24  // строка «+N ещё» под колонками
	// оценка ширины символа (13px) для усечения длинных имён — как
	// fitFlameLabel, но шрифт пропорциональный, коэффициент больше.
	depsMapCharW = 7.2
)

// лишние зависимости — в пометку «+N ещё» и в таблицу; кап применяется до
// раскладки по сторонам, поэтому колонки могут выйти разной длины.
const depsMapNodeCap = 16

// раскладка детерминирована: порядок узлов и сторона не зависят от времени
// или итерации map — два рендера одних данных дают идентичный вывод.
func dependencyMapSVG(ctx context.Context, deps []templates.DependencyRow, w int) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, wr io.Writer) error {
		shown := deps
		if len(shown) > depsMapNodeCap {
			shown = shown[:depsMapNodeCap]
		}
		var left, right []templates.DependencyRow
		for _, d := range shown {
			if d.Kind == "http" {
				right = append(right, d)
			} else {
				left = append(left, d)
			}
		}
		more := len(deps) - len(shown)
		h := max(len(left), len(right))*depsMapPitch + depsMapPadH
		h = max(h, depsMapMinH)
		if more > 0 {
			h += depsMapMoreH
		}
		cx, cy := w/2, h/2

		var sb strings.Builder
		sb.WriteString(svgRoot("deps-map", w, h, i18n.T(ctx, "region.deps.map")))
		sb.WriteString(depsMarkerDefs())
		// Рёбра — до узлов, чтобы прямоугольники перекрывали концы кривых.
		depsColumnSVG(&sb, left, depsMapLeftX, cx-depsMapHubW/2, cx, cy, h)
		depsColumnSVG(&sb, right, depsMapRightX, cx+depsMapHubW/2, cx, cy, h)
		hubX, hubY := cx-depsMapHubW/2, cy-depsMapHubH/2
		sb.WriteString(fmt.Sprintf(`<g class="deps-node deps-center"><rect x="%d" y="%d" rx="6" width="%d" height="%d"/><text class="deps-node-name" x="%d" y="%d" text-anchor="middle">%s</text></g>`,
			hubX, hubY, depsMapHubW, depsMapHubH, cx, cy+5, templ.EscapeString(i18n.T(ctx, "deps.center"))))
		if more > 0 {
			label := i18n.Tf(ctx, "deps.map_more", "n", strconv.Itoa(more))
			sb.WriteString(fmt.Sprintf(`<text class="deps-more" x="%d" y="%d" text-anchor="middle">%s</text>`,
				cx, h-8, templ.EscapeString(label)))
		}
		sb.WriteString(`</svg>`)
		_, err := io.WriteString(wr, sb.String())
		return err
	})
}

// колонка центрирована по вертикали (y_i = (h-n·pitch)/2 + i·pitch); порт на
// хабе — своя доля высоты на ребро, чтобы кривые не сходились в одной точке.
func depsColumnSVG(sb *strings.Builder, nodes []templates.DependencyRow, nodeX, hubEdgeX, cx, cy, h int) {
	n := len(nodes)
	if n == 0 {
		return
	}
	nodeEdgeX := nodeX + depsMapNodeW
	if nodeX > cx {
		nodeEdgeX = nodeX
	}
	midX := float64(hubEdgeX+nodeEdgeX) / 2
	y0 := (h-n*depsMapPitch)/2 + depsMapNodeDY
	for i, d := range nodes {
		top := y0 + i*depsMapPitch
		nodeCY := float64(top + depsMapNodeH/2)
		portY := float64(cy-depsMapHubH/2) + (float64(i)+0.5)*float64(depsMapHubH)/float64(n)
		tone := depsTone(d.ErrorRate)
		class := "deps-edge"
		if tone != "ok" {
			class += " deps-edge-" + tone
		}
		markers := ""
		if d.Direction == "in" || d.Direction == "both" {
			markers += fmt.Sprintf(` marker-start="url(#deps-arrow-%s)"`, tone)
		}
		if d.Direction == "out" || d.Direction == "both" {
			markers += fmt.Sprintf(` marker-end="url(#deps-arrow-%s)"`, tone)
		}
		title := fmt.Sprintf("%s: %d · p50 %s · p95 %s · %s", d.Target, d.Calls, depMicros(d.P50US), depMicros(d.P95US), depPercent(d.ErrorRate))
		sb.WriteString(fmt.Sprintf(`<path class="%s" d="M %d %s C %s %s %s %s %d %s"%s><title>%s</title></path>`,
			class, hubEdgeX, formatCoord(portY), formatCoord(midX), formatCoord(portY),
			formatCoord(midX), formatCoord(nodeCY), nodeEdgeX, formatCoord(nodeCY), markers, templ.EscapeString(title)))
		sb.WriteString(depNodeSVG(nodeX, top, d))
	}
}

func depNodeSVG(x, top int, d templates.DependencyRow) string {
	name, truncated := depFitName(d.Target)
	var sb strings.Builder
	sb.WriteString(`<g class="deps-node">`)
	if truncated {
		sb.WriteString(`<title>` + templ.EscapeString(d.Target) + `</title>`)
	}
	meta := fmt.Sprintf("%s · p95 %s · %s", depCount(d.Calls), depMicros(d.P95US), depPercent(d.ErrorRate))
	sb.WriteString(fmt.Sprintf(`<rect x="%d" y="%d" rx="6" width="%d" height="%d"/><text class="deps-node-name" x="%d" y="%d">%s</text><text class="deps-node-meta" x="%d" y="%d">%s</text></g>`,
		x, top, depsMapNodeW, depsMapNodeH, x+10, top+18, templ.EscapeString(name), x+10, top+34, templ.EscapeString(meta)))
	return sb.String()
}

// второй результат — было ли усечение (тогда полное имя уходит в <title>).
func depFitName(name string) (string, bool) {
	// минус 20: отступы по 10 с каждой стороны.
	textW := float64(depsMapNodeW - 20)
	fit := int(textW / depsMapCharW)
	if len([]rune(name)) <= fit {
		return name, false
	}
	return truncateRunes(name, fit-1) + "…", true
}

func depsTone(errorRate float64) string {
	switch {
	case errorRate >= 0.05:
		return "bad"
	case errorRate > 0:
		return "warn"
	default:
		return "ok"
	}
}

// refX = ширине маркера — остриё встаёт ровно на конце пути; auto-start-reverse
// разворачивает маркер на marker-start, иначе стрелка у хаба смотрела бы от него.
func depsMarkerDefs() string {
	var sb strings.Builder
	sb.WriteString(`<defs>`)
	for _, tone := range []string{"ok", "warn", "bad"} {
		sb.WriteString(fmt.Sprintf(`<marker id="deps-arrow-%s" class="deps-arrow-%s" viewBox="0 0 10 8" refX="10" refY="4" markerWidth="10" markerHeight="8" markerUnits="userSpaceOnUse" orient="auto-start-reverse"><path d="M 0 0 L 10 4 L 0 8 Z"/></marker>`, tone, tone))
	}
	sb.WriteString(`</defs>`)
	return sb.String()
}

// локальный дубль formatDurationUS — package web не достаёт до вспомогательных
// функций package templates.
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

// порог 999_950, не 999_999 — иначе строка отображалась бы как «1000.0k».
func depCount(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 999_950:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// локальный аналог formatFailureRate — недостижим из package web.
func depPercent(rate float64) string {
	return fmt.Sprintf("%.1f%%", rate*100)
}
