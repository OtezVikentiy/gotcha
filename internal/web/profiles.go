package web

import (
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func profilesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/profiles"
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
	tr := h.resolveTimeRange(w, r, "24h")
	environment := r.URL.Query().Get("environment")
	services, err := h.Profiles.ListServices(r.Context(), projectID, environment, tr.From, tr.To)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	_ = templates.ProfilesList(projectID, services, timeRangeVM(tr), environment, h.currentEmail(r)).Render(r.Context(), w)
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
	tr := h.resolveTimeRange(w, r, "24h")
	service := q.Get("service")
	profileType := q.Get("type")
	environment := q.Get("environment")
	transaction := q.Get("transaction")
	root, err := h.Profiles.Flame(r.Context(), projectID, service, environment, profileType, transaction, tr.From, tr.To)
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
		Range:       timeRangeVM(tr),
		Chart:       flamegraphSVG(r.Context(), root, 960),
	}
	_ = templates.ProfileFlame(vm, h.currentEmail(r)).Render(r.Context(), w)
}
