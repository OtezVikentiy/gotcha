package web

import (
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func incidentsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/incidents"
}

const incidentsPerPage = 50

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
	// nil в стендах без подсистемы мониторинга — 404, не паника при разыменовании.
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !canAccess {
		h.notFound(w, r)
		return
	}

	// CanOperate — read-only: подтверждение доступно оператору, просмотр — любому с доступом
	// к проекту, отказ не должен ронять страницу 404.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	page := parsePage(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	incidents, total, err := h.Uptime.IncidentsPaged(r.Context(), projectID, incidentsPerPage, (page-1)*incidentsPerPage)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	// Инцидент хранит только monitor_id — имя достаём из List проекта, не Get на каждый.
	monitors, err := h.Uptime.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	names := make(map[int64]string, len(monitors))
	for _, m := range monitors {
		names[m.ID] = m.Name
	}

	ackedByIDs := make([]int64, 0, len(incidents))
	for _, inc := range incidents {
		if inc.AcknowledgedBy != nil {
			ackedByIDs = append(ackedByIDs, *inc.AcknowledgedBy)
		}
	}
	ackedBy, err := h.ackedByEmails(r.Context(), ackedByIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	rows := make([]templates.IncidentRow, len(incidents))
	for i, inc := range incidents {
		rows[i] = templates.IncidentRow{Incident: inc, MonitorName: names[inc.MonitorID]}
	}

	_ = templates.IncidentsList(projectID, rows, page, total, h.currentEmail(r),
		templates.IncidentsListOpts{CanOperate: canOperate, AckedBy: ackedBy}).Render(r.Context(), w)
}
