package web

import (
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func profilesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/profiles"
}

// profilePeriodWindow — окно для query-параметра period на страницах профилей.
func profilePeriodWindow(period string) (time.Duration, string) {
	switch period {
	case "1h":
		return time.Hour, "1h"
	case "7d":
		return 7 * 24 * time.Hour, "7d"
	default:
		return 24 * time.Hour, "24h"
	}
}

// profilesList — GET /projects/{id}/profiles: перечень групп профилей за период.
func (h *Handler) profilesList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Profiles == nil {
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
	window, period := profilePeriodWindow(r.URL.Query().Get("period"))
	environment := r.URL.Query().Get("environment")
	now := time.Now().UTC()
	services, err := h.Profiles.ListServices(r.Context(), projectID, environment, now.Add(-window), now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	_ = templates.ProfilesList(projectID, services, period, environment, h.currentEmail(r)).Render(r.Context(), w)
}

// profileFlame — GET /projects/{id}/profiles/flame: flamegraph по фильтрам.
func (h *Handler) profileFlame(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Profiles == nil {
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
	q := r.URL.Query()
	window, period := profilePeriodWindow(q.Get("period"))
	service := q.Get("service")
	profileType := q.Get("type")
	environment := q.Get("environment")
	transaction := q.Get("transaction")
	now := time.Now().UTC()
	root, err := h.Profiles.Flame(r.Context(), projectID, service, environment, profileType, transaction, now.Add(-window), now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	vm := templates.ProfileFlameVM{
		ProjectID:   projectID,
		Service:     service,
		Type:        profileType,
		Transaction: transaction,
		Environment: environment,
		Period:      period,
		Chart:       flamegraphSVG(r.Context(), root, 960),
	}
	_ = templates.ProfileFlame(vm, h.currentEmail(r)).Render(r.Context(), w)
}
