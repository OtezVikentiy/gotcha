package web

import (
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// regressionsPreFilterLimit — потолок выборки из List ДО фильтрации по статусу
// в Go. RegressionService.List не принимает статус, а страница по умолчанию
// показывает только открытые, поэтому берём с запасом (открытых на цель — не
// более одной, закрытых со временем накапливается больше), чтобы фильтр open не
// оставался пустым из-за преобладания resolved в начале ORDER BY started_at DESC.
const regressionsPreFilterLimit = 500

// regressionsListLimit — сколько строк показываем после фильтрации по статусу.
const regressionsListLimit = 100

func regressionsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/regressions"
}

// regressionStatusFilter переводит query-параметр status в имя фильтра для формы
// и предикат для отбора в Go. Дефолт (пустой или неизвестный) — open: страница
// по умолчанию показывает то, что сейчас регрессирует. "all" — без фильтра.
func regressionStatusFilter(v string) (name string, keep func(status string) bool) {
	switch v {
	case "resolved":
		return "resolved", func(s string) bool { return s == "resolved" }
	case "all":
		return "all", func(string) bool { return true }
	default:
		return "open", func(s string) bool { return s == "open" }
	}
}

// regressionsList — GET /projects/{id}/regressions: таблица регрессий
// производительности проекта (цель, метрика, рост %, база→пик, статус,
// длительность). Доступ — CanAccessProject, иначе 404 (тот же принцип, что и у
// perfIssuesList); только чтение, POST'ов и sameOrigin здесь нет — регрессии
// закрываются оценщиком автоматически.
func (h *Handler) regressionsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.Regressions может быть nil в стендах без детекции — тогда 404, как и при
	// отсутствии доступа (тот же приём, что и nil-guard на h.PerfIssues), а не
	// паника при разыменовании.
	if h.Regressions == nil {
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

	filterName, keep := regressionStatusFilter(r.URL.Query().Get("status"))
	all, err := h.Regressions.List(r.Context(), projectID, regressionsPreFilterLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Фильтр по статусу — в Go: List статус не принимает (см. брифинг), поэтому
	// отбираем нужные и режем до потолка отображения.
	items := make([]trace.Regression, 0, len(all))
	for _, reg := range all {
		if keep(reg.Status) {
			items = append(items, reg)
			if len(items) >= regressionsListLimit {
				break
			}
		}
	}

	_ = templates.RegressionsList(projectID, items, filterName, h.currentEmail(r)).Render(r.Context(), w)
}
