package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const (
	monitorsListWindow  = 24 * time.Hour
	monitorsListBuckets = 24
)

const (
	monitorDetailChecksLimit      = 50
	monitorDetailIncidentsPerPage = 20
)

// данные из сырых проверок, выравнивание не нужно (align=0), только пол 5 мин.
const monitorLatencyBuckets = 48

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

func monitorStatus(m uptime.Monitor, states []uptime.State, inMaintenance bool) string {
	if !m.Enabled {
		return "paused"
	}
	if inMaintenance {
		return "maintenance"
	}
	return uptime.Aggregate(m, states)
}

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

// грубое приближение: взвешено поровну по бакетам, а не по числу проверок в каждом.
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

// org.ErrNotMember не должен ронять страницу — юзер мог получить доступ к проекту
// только через команду, не будучи членом организации.
func (h *Handler) canManageOrg(ctx context.Context, orgID, userID int64) (bool, error) {
	role, err := h.Org.Role(ctx, orgID, userID)
	if err != nil && !errors.Is(err, org.ErrNotMember) {
		return false, err
	}
	return role == org.RoleOwner || role == org.RoleAdmin, nil
}

func (h *Handler) canManageProject(ctx context.Context, projectID, userID int64) (bool, error) {
	orgID, err := h.Org.ProjectOrg(ctx, projectID)
	if err != nil {
		return false, err
	}
	return h.canManageOrg(ctx, orgID, userID)
}

func (h *Handler) monitorsList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.Uptime/h.UptimeQuery == nil в стендах без подсистемы мониторинга — 404, а не паника.
	if h.Uptime == nil || h.UptimeQuery == nil {
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

	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	monitors, err := h.Uptime.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	now := time.Now().UTC()
	from := now.Add(-monitorsListWindow)

	inMaintenance, err := h.Uptime.InMaintenance(r.Context(), projectID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	ids := make([]int64, len(monitors))
	for i, m := range monitors {
		ids[i] = m.ID
	}
	// пакетные запросы по всему набору мониторов вместо N+1 в цикле.
	statesByMon, err := h.Uptime.StatesBatch(r.Context(), ids)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	// отказ CH не роняет список: строки со статусами показываем, колонки статистики — «нет данных».
	uptimeStats, latencyByMon, barsByMon, statsFailed := h.monitorsListStats(r.Context(), projectID, ids, from, now)

	rows := make([]templates.MonitorRow, len(monitors))
	for i, m := range monitors {
		states := statesByMon[m.ID]
		latencyPoints := latencyByMon[m.ID]
		bars := barsByMon[m.ID]

		rows[i] = templates.MonitorRow{
			Monitor:      m,
			Status:       monitorStatus(m, states, inMaintenance),
			Uptime24h:    uptimeStats[m.ID],
			AvgLatencyMs: avgLatencyMs(latencyPoints),
			Bars:         availabilityBarsSVG(r.Context(), bars, availabilityBarsWidth, availabilityBarsHeight),
			LastChecked:  latestCheckedAt(states),
		}
	}

	_ = templates.MonitorsList(projectID, rows, canOperate, h.currentEmail(r), statsFailed).Render(r.Context(), w)
}

// любой отказ возвращает failed=true и пустые карты (nil-карта читается как «нет данных»).
func (h *Handler) monitorsListStats(ctx context.Context, projectID int64, ids []int64, from, now time.Time) (map[int64]uptime.UptimeStat, map[int64][]uptime.LatencyPoint, map[int64][]uptime.UptimeStat, bool) {
	uptimeStats, err := h.UptimeQuery.UptimeBatch(ctx, ids, from, now)
	if err != nil {
		slog.Warn("web: monitors list stats failed", "project_id", projectID, "query", "uptime", "error", err)
		return nil, nil, nil, true
	}
	latencyByMon, err := h.UptimeQuery.LatencyBatch(ctx, ids, from, now, monitorsListWindow/monitorsListBuckets)
	if err != nil {
		slog.Warn("web: monitors list stats failed", "project_id", projectID, "query", "latency", "error", err)
		return nil, nil, nil, true
	}
	barsByMon, err := h.UptimeQuery.BarsBatch(ctx, ids, from, now, monitorsListBuckets)
	if err != nil {
		slog.Warn("web: monitors list stats failed", "project_id", projectID, "query", "bars", "error", err)
		return nil, nil, nil, true
	}
	return uptimeStats, latencyByMon, barsByMon, false
}

// монитор не существует и монитор существует, но проект чужой — оба случая 404:
// не палим существование чужих числовых id.
func (h *Handler) loadAccessibleMonitor(w http.ResponseWriter, r *http.Request, uid int64) (uptime.Monitor, bool) {
	monitorID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return uptime.Monitor{}, false
	}
	m, err := h.Uptime.Get(r.Context(), monitorID)
	if err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.notFound(w, r)
			return uptime.Monitor{}, false
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return uptime.Monitor{}, false
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, m.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return uptime.Monitor{}, false
	}
	if !canAccess {
		h.notFound(w, r)
		return uptime.Monitor{}, false
	}
	return m, true
}

// в отличие от списочной колонки (UptimeBatch, сырой аптайм без исключений), страница
// монитора исключает интервалы окон обслуживания из подсчёта.
func (h *Handler) monitorUptimeStat(ctx context.Context, monitorID int64, windows []uptime.Window, from, to time.Time) (uptime.UptimeStat, error) {
	exclude := uptime.WindowIntervals(windows, from, to)
	return h.UptimeQuery.Uptime(ctx, monitorID, from, to, exclude)
}

func (h *Handler) monitorDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Uptime == nil || h.UptimeQuery == nil {
		h.notFound(w, r)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}

	canOperate, err := h.canOperateProject(r.Context(), m.ProjectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderMonitorDetail(w, r, m, canOperate)
}

// для показа heartbeat-URL один раз сразу после создания/ротации вызывающий выставляет
// сырой токен в m.HeartbeatToken (в БД — только sha256); при обычном GET поле пустое.
// Единственный уровень прав здесь — оператор; звать его canManage обещало бы уровень админа.
func (h *Handler) renderMonitorDetail(w http.ResponseWriter, r *http.Request, m uptime.Monitor, canOperate bool) {
	states, err := h.Uptime.States(r.Context(), m.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	now := time.Now().UTC()
	inMaintenance, err := h.Uptime.InMaintenance(r.Context(), m.ProjectID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	status := monitorStatus(m, states, inMaintenance)

	windows, err := h.Uptime.Windows(r.Context(), m.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	tr := h.resolveTimeRange(w, r, "24h")
	latencyStep := autoStep(tr.Window(), 5*time.Minute, 0, monitorLatencyBuckets)

	// отказ ClickHouse не роняет страницу: шапка/статус/инциденты (PostgreSQL) остаются,
	// на месте графика — «данные временно недоступны».
	var (
		uptime24h, uptime7d, uptime30d uptime.UptimeStat
		latencyPoints                  []uptime.LatencyPoint
		checks                         []uptime.CheckRow
	)
	statsErr := func() error {
		var err error
		if uptime24h, err = h.monitorUptimeStat(r.Context(), m.ID, windows, now.Add(-24*time.Hour), now); err != nil {
			return err
		}
		if uptime7d, err = h.monitorUptimeStat(r.Context(), m.ID, windows, now.Add(-7*24*time.Hour), now); err != nil {
			return err
		}
		if uptime30d, err = h.monitorUptimeStat(r.Context(), m.ID, windows, now.Add(-30*24*time.Hour), now); err != nil {
			return err
		}
		if latencyPoints, err = h.UptimeQuery.Latency(r.Context(), m.ID, tr.From, tr.To, latencyStep); err != nil {
			return err
		}
		checks, err = h.UptimeQuery.Recent(r.Context(), m.ID, monitorDetailChecksLimit)
		return err
	}()
	statsFailed := statsErr != nil
	if statsFailed {
		slog.Warn("web: monitor detail stats failed", "monitor_id", m.ID, "error", statsErr)
	}
	latencyPoints = fillSeries(latencyPoints, tr.From, tr.To, latencyStep,
		func(p uptime.LatencyPoint) time.Time { return p.T },
		func(t time.Time) uptime.LatencyPoint { return uptime.LatencyPoint{T: t} })
	var deploys []deploy.Deployment
	if h.Deploy != nil {
		deploys, _ = h.Deploy.List(r.Context(), m.ProjectID, tr.From, tr.To, 20)
	}
	latencyChart := latencyStackedSVG(r.Context(), latencyPoints, deploys, latencyChartWidth, latencyChartHeight)

	incPage := parsePage(r.URL.Query().Get("incpage"))
	if incPage < 1 {
		incPage = 1
	}
	incidents, incTotal, err := h.Uptime.IncidentsForMonitorPaged(r.Context(), m.ID, monitorDetailIncidentsPerPage, (incPage-1)*monitorDetailIncidentsPerPage)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	_ = templates.MonitorDetail(m, status, uptime24h, uptime7d, uptime30d, latencyChart, timeRangeVM(tr), checks, incidents, incPage, incTotal, canOperate, h.BaseURL, h.currentEmail(r), statsFailed).Render(r.Context(), w)
}

func (h *Handler) monitorSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}
	if _, ok := h.requireProjectOperator(w, r, m.ProjectID, uid); !ok {
		return
	}
	if err := h.Uptime.SetEnabled(r.Context(), m.ID, enabled); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if enabled {
		h.flashOK(w, "flash.monitor_resumed", 0)
	} else {
		h.flashOK(w, "flash.monitor_paused", 0)
	}
	http.Redirect(w, r, monitorDetailPath(m.ID), http.StatusSeeOther)
}

func (h *Handler) monitorPause(w http.ResponseWriter, r *http.Request) {
	h.monitorSetEnabled(w, r, false)
}

func (h *Handler) monitorResume(w http.ResponseWriter, r *http.Request) {
	h.monitorSetEnabled(w, r, true)
}

func (h *Handler) monitorDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}
	if _, ok := h.requireProjectOperator(w, r, m.ProjectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	// CSP без unsafe-inline не исполняет inline confirm() — подтверждение отдельной страницей.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.monitor_delete.message", "confirm.delete",
			monitorDetailPath(m.ID), monitorDeletePath(m.ID), nil,
			"name", m.Name)
		return
	}
	if err := h.Uptime.Delete(r.Context(), m.ID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, monitorsPath(m.ProjectID), http.StatusSeeOther)
}
