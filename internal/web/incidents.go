package web

import (
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func incidentsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/incidents"
}

// incidentsListLimit — сколько последних инцидентов проекта показывать;
// тот же порядок величины, что и maxFailedDeliveries в alerts.go —
// обзорная лента, не полный архив.
const incidentsListLimit = 200

// incidentsList — GET /projects/{id}/incidents: инциденты по всем мониторам
// проекта, самые свежие первыми. Доступ — CanAccessProject (любой участник
// проекта, не только owner/admin), тот же принцип, что и у monitorsList.
func (h *Handler) incidentsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
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

	incidents, err := h.Uptime.Incidents(r.Context(), projectID, incidentsListLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	// Инцидент хранит только monitor_id — имя монитора для ссылки в таблице
	// достаём из List проекта, чтобы не делать отдельный Get на каждый
	// инцидент.
	monitors, err := h.Uptime.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	names := make(map[int64]string, len(monitors))
	for _, m := range monitors {
		names[m.ID] = m.Name
	}

	rows := make([]templates.IncidentRow, len(incidents))
	for i, inc := range incidents {
		rows[i] = templates.IncidentRow{Incident: inc, MonitorName: names[inc.MonitorID]}
	}

	_ = templates.IncidentsList(projectID, rows, h.currentEmail(r)).Render(r.Context(), w)
}
