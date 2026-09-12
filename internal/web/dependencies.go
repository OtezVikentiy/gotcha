package web

import (
	"log/slog"
	"net/http"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// +1 к depsLimit в запросе — чтобы узнать об усечении, не считая total отдельно.
const depsLimit = 50

func (h *Handler) dependencies(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// nil в стендах без трейсинга — тогда 404, как у performanceList.
	if h.Trace == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}
	tr := h.resolveTimeRange(w, r, perfDefaultPeriod)

	var loadFailed, truncated bool
	deps, err := h.Trace.Dependencies(r.Context(), projectID, tr.From, tr.To, depsLimit+1)
	if err != nil {
		slog.Warn("web: dependencies query failed", "project_id", projectID, "err", err)
		loadFailed = true
	}
	if len(deps) > depsLimit {
		deps = deps[:depsLimit]
		truncated = true
	}
	rows := make([]templates.DependencyRow, 0, len(deps))
	for _, d := range deps {
		rows = append(rows, templates.DependencyRow{
			Kind: d.Kind, Target: d.Target, Calls: d.Calls,
			P50US: d.P50US, P95US: d.P95US, ErrorRate: d.ErrorRate,
			Direction: string(d.Direction()),
		})
	}
	filter := templates.DepsFilter{Range: timeRangeVM(tr), Active: tr.Key != perfDefaultPeriod}
	// Шаблон не может вызвать web-функцию, поэтому SVG строится тут и передаётся компонентом.
	var mapSVG templ.Component = templ.NopComponent
	if !loadFailed && len(rows) > 0 {
		mapSVG = dependencyMapSVG(r.Context(), rows, depsMapWidth)
	}
	_ = templates.DependenciesScreen(projectID, rows, filter, mapSVG, loadFailed, truncated, h.currentEmail(r)).Render(r.Context(), w)
}
