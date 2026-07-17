package web

import (
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func profileRegressionsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/profile-regressions"
}

// profileRegressionStatusFilter нормализует query-статус (дефолт open).
func profileRegressionStatusFilter(v string) string {
	switch v {
	case "resolved":
		return "resolved"
	case "all":
		return "all"
	default:
		return "open"
	}
}

// profileRegressionsList — GET /projects/{id}/profile-regressions: таблица
// регрессий self-CPU функций. Доступ — CanAccessProject, чужим 404; только
// чтение (регрессии закрываются оценщиком автоматически).
func (h *Handler) profileRegressionsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.ProfileRegressions == nil {
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
	filter := profileRegressionStatusFilter(r.URL.Query().Get("status"))
	regs, err := h.ProfileRegressions.List(r.Context(), projectID, filter, 200)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	_ = templates.ProfileRegressionsList(projectID, regs, filter, h.currentEmail(r)).Render(r.Context(), w)
}
