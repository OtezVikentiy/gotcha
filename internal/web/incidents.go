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

// incidentsPerPage — размер страницы ленты инцидентов проекта. Раньше лента
// была капом на 200 без пагинации (длинный неограниченный список); теперь —
// постранично, чтобы страница не выгребала и не рендерила весь архив разом.
const incidentsPerPage = 50

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
	// h.Uptime может быть nil в стендах без подсистемы мониторинга — тогда
	// 404, а не паника при разыменовании (тот же guard, что и в metricsList).
	if h.Uptime == nil {
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

	// CanOperate — read-only, тем же приёмом, что HostDetail/incidentFeed:
	// подтверждение (W2-C находка 2) доступно оператору, просмотр — любому с
	// доступом к проекту, отказ не должен ронять всю страницу 404.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	page := parsePage(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	incidents, total, err := h.Uptime.IncidentsPaged(r.Context(), projectID, incidentsPerPage, (page-1)*incidentsPerPage)
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

	// ackedBy — W2-C находка 4: email подтвердившего, батчем на всю
	// страницу (см. ackedByEmails).
	ackedByIDs := make([]int64, 0, len(incidents))
	for _, inc := range incidents {
		if inc.AcknowledgedBy != nil {
			ackedByIDs = append(ackedByIDs, *inc.AcknowledgedBy)
		}
	}
	ackedBy, err := h.ackedByEmails(r.Context(), ackedByIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	rows := make([]templates.IncidentRow, len(incidents))
	for i, inc := range incidents {
		rows[i] = templates.IncidentRow{Incident: inc, MonitorName: names[inc.MonitorID]}
	}

	_ = templates.IncidentsList(projectID, rows, page, total, h.currentEmail(r),
		templates.IncidentsListOpts{CanOperate: canOperate, AckedBy: ackedBy}).Render(r.Context(), w)
}
