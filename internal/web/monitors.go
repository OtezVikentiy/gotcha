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
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
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

// monitorDetailChecksLimit — сколько последних проверок показывает страница
// монитора. monitorDetailIncidentsPerPage — размер страницы ленты инцидентов
// на детали монитора (постранично через ?incpage=N, а не единый кап-список).
const (
	monitorDetailChecksLimit      = 50
	monitorDetailIncidentsPerPage = 20
)

// monitorLatencyBuckets — целевое число точек графика задержек монитора; шаг
// подбирает autoStep по выбранному окну (для 24ч по умолчанию — 30м). Данные
// из сырых проверок, поэтому выравнивание не нужно (align=0), только пол 5 мин.
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

// canManageOrg — owner/admin организации orgID. org.ErrNotMember не должен
// ронять страницу (юзер мог получить доступ к проекту только через команду) —
// тот же приём, что и canManage в issuesList. Вынесена из canManageProject
// (находка B4): requireProjectOperator уже знает orgID и вызывает эту часть
// напрямую, без повторного резолва projectID -> orgID.
func (h *Handler) canManageOrg(ctx context.Context, orgID, userID int64) (bool, error) {
	role, err := h.Org.Role(ctx, orgID, userID)
	if err != nil && !errors.Is(err, org.ErrNotMember) {
		return false, err
	}
	return role == org.RoleOwner || role == org.RoleAdmin, nil
}

// canManageProject — owner/admin организации проекта.
func (h *Handler) canManageProject(ctx context.Context, projectID, userID int64) (bool, error) {
	orgID, err := h.Org.ProjectOrg(ctx, projectID)
	if err != nil {
		return false, err
	}
	return h.canManageOrg(ctx, orgID, userID)
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
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.Uptime/h.UptimeQuery могут быть nil в стендах без подсистемы
	// мониторинга — тогда 404 (как несуществующая фича), а не паника при
	// разыменовании (тот же guard, что и h.Metrics в metricsList).
	if h.Uptime == nil || h.UptimeQuery == nil {
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

	// С задачи 2 (спека 2026-08-08) кнопка «New monitor» — операторская, не
	// owner/admin-only: canOperate наполняется canOperateProject, а не
	// canManageProject.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	monitors, err := h.Uptime.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	now := time.Now().UTC()
	from := now.Add(-monitorsListWindow)

	inMaintenance, err := h.Uptime.InMaintenance(r.Context(), projectID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	ids := make([]int64, len(monitors))
	for i, m := range monitors {
		ids[i] = m.ID
	}
	// Пакетные запросы по всему набору мониторов вместо N+1 в цикле: uptime,
	// состояния (PG), латентность и полоски доступности (CH) — по одному запросу
	// на всех (списочная страница иначе делала ~3N round-trip, из них ~2N в CH).
	statesByMon, err := h.Uptime.StatesBatch(r.Context(), ids)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Аптайм, задержка и полосы — из ClickHouse; мониторы и их состояния —
	// из PostgreSQL. Отказ CH не роняет список (единый приём CH-страниц,
	// образец — logsList): строки со статусами показываем, колонки статистики
	// — «нет данных», над таблицей — «статистика временно недоступна».
	// Первый отказ прекращает опрос хранилища.
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

// monitorsListStats — три батч-запроса списка мониторов к ClickHouse. Любой
// отказ возвращает failed=true и пустые карты (nil-карта читается как
// «нет данных» для каждого монитора); ошибка уходит в лог, не в ответ.
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

// loadAccessibleMonitor — общая часть GET/POST monitor-обработчиков: находит
// монитор по id и проверяет, что текущий юзер видит его проект. Оба случая
// (монитор не существует, монитор существует но проект чужой) отдают 404 —
// не палим существование чужих числовых id, тот же принцип, что и в
// loadAccessibleIssue.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return uptime.Monitor{}, false
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, m.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return uptime.Monitor{}, false
	}
	if !canAccess {
		h.notFound(w, r)
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
// Pause/Resume/Edit/Delete (с задачи 2 — оператору проекта, не только
// owner/admin).
func (h *Handler) monitorDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// nil-guard до loadAccessibleMonitor (сам дереференсит h.Uptime) и до
	// renderMonitorDetail (дереференсит h.UptimeQuery): в стендах без
	// мониторинга — 404, а не паника (тот же класс, что и traceWaterfall).
	if h.Uptime == nil || h.UptimeQuery == nil {
		h.notFound(w, r)
		return
	}
	m, ok := h.loadAccessibleMonitor(w, r, uid)
	if !ok {
		return
	}

	// С задачи 2 обе группы флагов на странице детали — операторские: и
	// Pause/Resume/Edit/Delete (canManage), и карточка heartbeat-токена
	// (canOperate). Один и тот же canOperateProject наполняет оба параметра —
	// разошлись бы они только если предикаты «управлять» и «оперировать»
	// когда-нибудь разъедутся (см. canOperateProject в operate.go).
	canOperate, err := h.canOperateProject(r.Context(), m.ProjectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.renderMonitorDetail(w, r, m, canOperate, canOperate)
}

// renderMonitorDetail собирает и рендерит страницу детали монитора. Для показа
// heartbeat-URL один раз сразу после создания/ротации токена вызывающий выставляет
// сырой токен в m.HeartbeatToken (в БД хранится только его sha256): при обычном
// GET поле пустое и URL пинга не рендерится — нужно перегенерировать токен.
// canManage гейтит кнопки Pause/Resume/Edit/Delete; canOperate отдельно
// гейтит карточку heartbeat-токена. С задачи 2 оба параметра у всех вызывающих
// наполняются одним и тем же canOperateProject (весь набор кнопок монитора —
// операторский, спека 2026-08-08); флаги остаются раздельными в сигнатуре на
// случай, если предикаты «управлять» и «оперировать» разойдутся в будущем.
func (h *Handler) renderMonitorDetail(w http.ResponseWriter, r *http.Request, m uptime.Monitor, canManage, canOperate bool) {
	states, err := h.Uptime.States(r.Context(), m.ID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	now := time.Now().UTC()
	inMaintenance, err := h.Uptime.InMaintenance(r.Context(), m.ProjectID, now)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	status := monitorStatus(m, states, inMaintenance)

	windows, err := h.Uptime.Windows(r.Context(), m.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	tr := h.resolveTimeRange(w, r, "24h")
	latencyStep := autoStep(tr.Window(), 5*time.Minute, 0, monitorLatencyBuckets)

	// Все чтения ClickHouse карточки (аптайм за три окна, задержки, последние
	// проверки) — одним блоком: отказ хранилища не роняет страницу (единый
	// приём CH-страниц, образец — logsList), шапка, статус, действия и
	// инциденты (PostgreSQL) остаются, на месте графика и проверок — «данные
	// временно недоступны». Первый отказ прекращает опрос хранилища.
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
	// Маркеры деплоев на графике задержек монитора (C5): выкладки проекта в
	// том же выбранном окне графика.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	_ = templates.MonitorDetail(m, status, uptime24h, uptime7d, uptime30d, latencyChart, timeRangeVM(tr), checks, incidents, incPage, incTotal, canManage, canOperate, h.BaseURL, h.currentEmail(r), statsFailed).Render(r.Context(), w)
}

// monitorSetEnabled — общая часть POST /monitors/{id}/pause и /resume:
// sameOrigin + requireProjectOperator (оператор, спека 2026-08-08) →
// SetEnabled → 303 обратно на страницу монитора.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Flash различает паузу и возобновление (K7-9): «Сохранено» здесь не
	// сказало бы, в каком состоянии монитор остался.
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

// monitorDelete — POST /monitors/{id}/delete: sameOrigin +
// requireProjectOperator (оператор, спека 2026-08-08) → Delete → 303 на
// список мониторов проекта.
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
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения вместо необратимого действия.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.monitor_delete.message", "confirm.delete",
			monitorDetailPath(m.ID), monitorDeletePath(m.ID), nil,
			"name", m.Name)
		return
	}
	if err := h.Uptime.Delete(r.Context(), m.ID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, monitorsPath(m.ProjectID), http.StatusSeeOther)
}
