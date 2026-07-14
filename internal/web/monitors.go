package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// monitorsListWindow/Buckets — окно и разрешение полоски доступности и
// uptime%/latency в списке мониторов: 24 часа, 24 корзины (одна на час).
const (
	monitorsListWindow  = 24 * time.Hour
	monitorsListBuckets = 24
)

// monitorDetailChecksLimit/IncidentsLimit — сколько последних проверок и
// инцидентов показывает страница монитора.
const (
	monitorDetailChecksLimit    = 50
	monitorDetailIncidentsLimit = 50
)

// monitorLatencyStep — шаг графика задержек за 24 часа на странице монитора
// (24 точки, одна на час — тот же шаг, что и у полоски доступности списка).
const monitorLatencyStep = time.Hour

func monitorsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/monitors"
}

func monitorDetailPath(monitorID int64) string {
	return "/monitors/" + strconv.FormatInt(monitorID, 10)
}

func monitorPausePath(monitorID int64) string {
	return monitorDetailPath(monitorID) + "/pause"
}

func monitorResumePath(monitorID int64) string {
	return monitorDetailPath(monitorID) + "/resume"
}

func monitorDeletePath(monitorID int64) string {
	return monitorDetailPath(monitorID) + "/delete"
}

// monitorStatus — статус монитора для отображения: enabled=false → "paused";
// активное окно обслуживания проекта → "maintenance"; иначе агрегат по
// consensus-политике монитора (uptime.Aggregate) — тот же приоритет, что
// требует спека задачи 2 (не дублировать consensus-логику детектора).
func monitorStatus(m uptime.Monitor, states []uptime.State, inMaintenance bool) string {
	if !m.Enabled {
		return "paused"
	}
	if inMaintenance {
		return "maintenance"
	}
	return uptime.Aggregate(m, states)
}

// latestCheckedAt возвращает самый свежий LastCheckedAt среди states монитора
// (регион с самой недавней проверкой), либо nil, если ни один регион ещё не
// проверялся (свежесозданный монитор).
func latestCheckedAt(states []uptime.State) *time.Time {
	var latest *time.Time
	for _, st := range states {
		if st.LastCheckedAt == nil {
			continue
		}
		if latest == nil || st.LastCheckedAt.After(*latest) {
			latest = st.LastCheckedAt
		}
	}
	return latest
}

// avgLatencyMs усредняет AvgTotalMs по непустым бакетам points — грубое, но
// достаточное для списочной колонки приближение "средней задержки за
// период" (Query не отдаёт единое агрегированное среднее одним вызовом,
// только временной ряд), взвешенное поровну по бакетам, а не по числу
// проверок в каждом.
func avgLatencyMs(points []uptime.LatencyPoint) uint32 {
	var sum uint64
	var count uint64
	for _, p := range points {
		if p.AvgTotalMs > 0 {
			sum += uint64(p.AvgTotalMs)
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return uint32(sum / count)
}

// canManageProject — owner/admin организации проекта. org.ErrNotMember не
// должен ронять страницу (юзер мог получить доступ к проекту только через
// команду) — тот же приём, что и canManage в issuesList.
func (h *Handler) canManageProject(ctx context.Context, projectID, userID int64) (bool, error) {
	orgID, err := h.Org.ProjectOrg(ctx, projectID)
	if err != nil {
		return false, err
	}
	role, err := h.Org.Role(ctx, orgID, userID)
	if err != nil && !errors.Is(err, org.ErrNotMember) {
		return false, err
	}
	return role == org.RoleOwner || role == org.RoleAdmin, nil
}

// monitorsList — GET /projects/{id}/monitors: таблица мониторов проекта
// (доступ — CanAccessProject, иначе 404, тот же принцип, что и у
// issuesList).
func (h *Handler) monitorsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !canAccess {
		http.NotFound(w, r)
		return
	}

	canManage, err := h.canManageProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	monitors, err := h.Uptime.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	now := time.Now().UTC()
	from := now.Add(-monitorsListWindow)

	inMaintenance, err := h.Uptime.InMaintenance(r.Context(), projectID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	ids := make([]int64, len(monitors))
	for i, m := range monitors {
		ids[i] = m.ID
	}
	uptimeStats, err := h.UptimeQuery.UptimeBatch(r.Context(), ids, from, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	rows := make([]templates.MonitorRow, len(monitors))
	for i, m := range monitors {
		states, err := h.Uptime.States(r.Context(), m.ID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}

		latencyPoints, err := h.UptimeQuery.Latency(r.Context(), m.ID, from, now, monitorsListWindow/monitorsListBuckets)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}

		bars, err := h.UptimeQuery.Bars(r.Context(), m.ID, from, now, monitorsListBuckets)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}

		rows[i] = templates.MonitorRow{
			Monitor:      m,
			Status:       monitorStatus(m, states, inMaintenance),
			Uptime24h:    uptimeStats[m.ID],
			AvgLatencyMs: avgLatencyMs(latencyPoints),
			Bars:         availabilityBarsSVG(bars, availabilityBarsWidth, availabilityBarsHeight),
			LastChecked:  latestCheckedAt(states),
		}
	}

	_ = templates.MonitorsList(projectID, rows, canManage, h.currentEmail(r)).Render(r.Context(), w)
}

// loadAccessibleMonitor — общая часть GET/POST monitor-обработчиков: находит
// монитор по id и проверяет, что текущий юзер видит его проект. Оба случая
// (монитор не существует, монитор существует но проект чужой) отдают 404 —
// не палим существование чужих числовых id, тот же принцип, что и в
// loadAccessibleIssue.
func (h *Handler) loadAccessibleMonitor(w http.ResponseWriter, r *http.Request, uid int64) (uptime.Monitor, bool) {
	monitorID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return uptime.Monitor{}, false
	}
	m, err := h.Uptime.Get(r.Context(), monitorID)
	if err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			http.NotFound(w, r)
			return uptime.Monitor{}, false
		}
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return uptime.Monitor{}, false
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, m.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return uptime.Monitor{}, false
	}
	if !canAccess {
		http.NotFound(w, r)
		return uptime.Monitor{}, false
	}
	return m, true
}

// monitorUptimeStat — uptime% монитора на [from,to), исключая интервалы окон
// обслуживания проекта (WindowIntervals из уже загруженных windows) — та
// часть спеки, которая отличает страницу монитора (skрытые окна) от
// списочной колонки (сырой аптайм за 24ч, UptimeBatch без исключений).
func (h *Handler) monitorUptimeStat(ctx context.Context, monitorID int64, windows []uptime.Window, from, to time.Time) (uptime.UptimeStat, error) {
	exclude := uptime.WindowIntervals(windows, from, to)
	return h.UptimeQuery.Uptime(ctx, monitorID, from, to, exclude)
}

// monitorDetail — GET /monitors/{id}: крупный статус, uptime% за
// 24ч/7д/30д (без окон обслуживания), stacked-график задержек за 24ч,
// последние 50 проверок, таймлайн инцидентов, SSL, кнопки
// Pause/Resume/Edit/Delete (только owner/admin).
func (h *Handler) monitorDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}

	canManage, err := h.canManageProject(r.Context(), m.ProjectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	states, err := h.Uptime.States(r.Context(), m.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	now := time.Now().UTC()
	inMaintenance, err := h.Uptime.InMaintenance(r.Context(), m.ProjectID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	status := monitorStatus(m, states, inMaintenance)

	windows, err := h.Uptime.Windows(r.Context(), m.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	uptime24h, err := h.monitorUptimeStat(r.Context(), m.ID, windows, now.Add(-24*time.Hour), now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	uptime7d, err := h.monitorUptimeStat(r.Context(), m.ID, windows, now.Add(-7*24*time.Hour), now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	uptime30d, err := h.monitorUptimeStat(r.Context(), m.ID, windows, now.Add(-30*24*time.Hour), now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	latencyPoints, err := h.UptimeQuery.Latency(r.Context(), m.ID, now.Add(-24*time.Hour), now, monitorLatencyStep)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	latencyChart := latencyStackedSVG(latencyPoints, latencyChartWidth, latencyChartHeight)

	checks, err := h.UptimeQuery.Recent(r.Context(), m.ID, monitorDetailChecksLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	incidents, err := h.Uptime.IncidentsForMonitor(r.Context(), m.ID, monitorDetailIncidentsLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	_ = templates.MonitorDetail(m, status, uptime24h, uptime7d, uptime30d, latencyChart, checks, incidents, canManage, h.BaseURL, h.currentEmail(r)).Render(r.Context(), w)
}

// monitorSetEnabled — общая часть POST /monitors/{id}/pause и /resume:
// sameOrigin + requireProjectRole (owner/admin) → SetEnabled → 303 обратно
// на страницу монитора.
func (h *Handler) monitorSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}
	if _, ok := h.requireProjectRole(w, r, m.ProjectID, uid); !ok {
		return
	}
	if err := h.Uptime.SetEnabled(r.Context(), m.ID, enabled); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, monitorDetailPath(m.ID), http.StatusSeeOther)
}

func (h *Handler) monitorPause(w http.ResponseWriter, r *http.Request) {
	h.monitorSetEnabled(w, r, false)
}

func (h *Handler) monitorResume(w http.ResponseWriter, r *http.Request) {
	h.monitorSetEnabled(w, r, true)
}

// monitorDelete — POST /monitors/{id}/delete: sameOrigin +
// requireProjectRole (owner/admin) → Delete → 303 на список мониторов
// проекта.
func (h *Handler) monitorDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}
	if _, ok := h.requireProjectRole(w, r, m.ProjectID, uid); !ok {
		return
	}
	if err := h.Uptime.Delete(r.Context(), m.ID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, monitorsPath(m.ProjectID), http.StatusSeeOther)
}
