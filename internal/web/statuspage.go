package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// фактическая глубина не превышает срок хранения проверок, см. statusPageDays.
const statusPageBuckets = 90

// срок хранения проверок делит ту же ручку GOTCHA_EVENT_RETENTION_DAYS, что и
// события; 0 означает «не задан», окно не урезается.
func (h *Handler) statusPageDays() int {
	if h.RetentionDays > 0 && h.RetentionDays < statusPageBuckets {
		return h.RetentionDays
	}
	return statusPageBuckets
}

const statusPageUpcomingWindow = 7 * 24 * time.Hour

const (
	statusPageIncidentsPerMonitor = 50
	statusPageIncidentsTotal      = 20
)

// кешируется только успех — 404 не кешируем, иначе включённая страница ждала
// бы TTL; размер карты ограничен против роста от перебора случайных ключей.
const (
	statusCacheTTL        = 30 * time.Second
	statusCacheMaxEntries = 100
)

// сборка переживает отмену запроса-ведущего — таймаут единственное, что её
// остановит при недоступном PG/CH.
const statusPageBuildTimeout = 10 * time.Second

type statusCacheEntry struct {
	view    templates.StatusPageView
	expires time.Time
}

// done закрывается после заполнения view/err — запись до close, чтение
// после, отдельный мьютекс на поля не нужен.
type statusBuild struct {
	done chan struct{}
	view templates.StatusPageView
	err  error
}

// нулевое значение готово к работе, карты создаются лениво; inflight — single-flight,
// иначе параллельные запросы на холодный ключ умножили бы сборку кратно их числу.
type statusCache struct {
	mu       sync.Mutex
	entries  map[string]statusCacheEntry
	inflight map[string]*statusBuild
}

// ctx принадлежит ждущему запросу, не сборке — у неё свой таймаут; иначе
// отвалившийся клиент оставлял бы горутину висеть на <-b.done навсегда.
func (c *statusCache) load(ctx context.Context, key string, now time.Time, build func() (templates.StatusPageView, error)) (templates.StatusPageView, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.view, nil
	}
	if b, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-b.done:
			return b.view, b.err
		case <-ctx.Done():
			return templates.StatusPageView{}, ctx.Err()
		}
	}
	b := &statusBuild{done: make(chan struct{})}
	if c.inflight == nil {
		c.inflight = make(map[string]*statusBuild)
	}
	c.inflight[key] = b
	c.mu.Unlock()

	b.view, b.err = build()

	c.mu.Lock()
	delete(c.inflight, key)
	if b.err == nil {
		// TTL отсчитывается от завершения сборки, не от захода в load — иначе
		// на медленной сборке запись жила бы заметно меньше 30с.
		c.putLocked(key, b.view, time.Now())
	}
	c.mu.Unlock()

	close(b.done)
	return b.view, b.err
}

func (c *statusCache) putLocked(key string, view templates.StatusPageView, now time.Time) {
	if c.entries == nil || len(c.entries) >= statusCacheMaxEntries {
		// сброс кеша целиком, не вытеснение одной записи — TTL и так 30с.
		c.entries = make(map[string]statusCacheEntry, statusCacheMaxEntries)
	}
	c.entries[key] = statusCacheEntry{view: view, expires: now.Add(statusCacheTTL)}
}

func (c *statusCache) invalidate(keys ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keys {
		delete(c.entries, key)
	}
}

func statusPagesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/statuspages"
}

// анонимная страница без авторизации — единственный такой браузерный роут (heartbeat лишь машинный).
// в HTML только display_name, статус, uptime%, полоска и инциденты без причины и региона.
func (h *Handler) statusPage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	now := time.Now().UTC()

	// buildCtx отвязан от отмены запроса (не рвёт сборку для других ждущих),
	// но ограничен таймаутом сам — подвисший PG/CH не держит его вечно.
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), statusPageBuildTimeout)
	defer cancel()

	view, err := h.statusCache.load(r.Context(), key, now, func() (templates.StatusPageView, error) {
		return h.buildStatusPage(buildCtx, key, now)
	})
	if err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			// found=false одинаково для отсутствующего ключа и выключенной
			// страницы — не палим разницу отдельной веткой.
			pubID, found, rerr := h.Uptime.StatusPageForRedirect(r.Context(), key)
			if rerr != nil {
				if r.Context().Err() != nil {
					// context.Canceled здесь не поломка — писать уже
					// некому, логировать как Error не за что.
					return
				}
				slog.Error("statusPage: redirect lookup failed", "error", rerr)
				h.renderError(w, r, http.StatusInternalServerError, "")
				return
			}
			if found {
				http.Redirect(w, r, "/status/"+pubID, http.StatusMovedPermanently)
				return
			}
			h.renderError(w, r, http.StatusNotFound, "")
			return
		}
		if r.Context().Err() != nil {
			// клиент отвалился — писать некому.
			return
		}
		// CH-отказ здесь сознательно 500, не деградация с заглушкой: страницу опрашивают поллеры,
		// закешированная заглушка выдавала бы отказ за «сервис в порядке» ещё 30с.
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderStatusPage(w, r, view)
}

// view общая для всех посетителей — Monitors копируется перед мутацией,
// SVG-полоска строится в копии.
func (h *Handler) renderStatusPage(w http.ResponseWriter, r *http.Request, view templates.StatusPageView) {
	if len(view.Monitors) > 0 {
		monitors := make([]templates.StatusMonitorView, len(view.Monitors))
		copy(monitors, view.Monitors)
		for i := range monitors {
			monitors[i].Bars = availabilityBarsSVG(r.Context(), monitors[i].BarStats,
				availabilityBarsWidth, availabilityBarsHeight)
		}
		view.Monitors = monitors
	}
	_ = templates.PublicStatusPage(view).Render(r.Context(), w)
}

// uptime% и полоска считаются как на странице монитора — окна обслуживания
// исключены из знаменателя.
func (h *Handler) buildStatusPage(ctx context.Context, publicID string, now time.Time) (templates.StatusPageView, error) {
	sp, spMonitors, err := h.Uptime.StatusPageByPublicID(ctx, publicID)
	if err != nil {
		return templates.StatusPageView{}, err
	}

	inMaintenance, err := h.Uptime.InMaintenance(ctx, sp.ProjectID, now)
	if err != nil {
		return templates.StatusPageView{}, err
	}
	windows, err := h.Uptime.Windows(ctx, sp.ProjectID)
	if err != nil {
		return templates.StatusPageView{}, err
	}

	days := h.statusPageDays()
	from := now.AddDate(0, 0, -days)
	exclude := uptime.WindowIntervals(windows, from, now)

	view := templates.StatusPageView{
		Title:       sp.Title,
		Description: sp.Description,
		// подписи страницы должны называть то окно, которое реально показано.
		Days: days,
	}

	// сортировка идёт по исходному времени, в модель уходит уже отформатированная
	// строка — закешированная страница рендерится байт-в-байт одинаково.
	type datedIncident struct {
		view templates.StatusIncidentView
		at   time.Time
	}

	// цикл ниже только раскладывает уже прочитанные пакетно карты по
	// мониторам — к БД внутри цикла не обращаемся.
	spIDs := make([]int64, len(spMonitors))
	for i, spm := range spMonitors {
		spIDs[i] = spm.MonitorID
	}
	statesByMon, err := h.Uptime.StatesBatch(ctx, spIDs)
	if err != nil {
		return templates.StatusPageView{}, err
	}
	// корзин ровно столько же, сколько суток в окне — иначе клетки за
	// пределами срока хранения выглядят как «данных нет».
	barsByMon, err := h.UptimeQuery.BarsBatch(ctx, spIDs, from, now, days)
	if err != nil {
		return templates.StatusPageView{}, err
	}
	monitorsByID, err := h.Uptime.GetBatch(ctx, spIDs)
	if err != nil {
		return templates.StatusPageView{}, err
	}
	// exclude — окна обслуживания проекта, посчитаны один раз до цикла и
	// переиспользуются для всех мониторов страницы.
	uptimeByMon, err := h.UptimeQuery.UptimeExcludingBatch(ctx, spIDs, from, now, exclude)
	if err != nil {
		return templates.StatusPageView{}, err
	}
	incidentsByMon, err := h.Uptime.IncidentsForMonitorsBatch(ctx, spIDs, statusPageIncidentsPerMonitor)
	if err != nil {
		return templates.StatusPageView{}, err
	}

	var down, counted int
	var incidents []datedIncident
	for _, spm := range spMonitors {
		m := monitorsByID[spm.MonitorID]
		// GetBatch не отличает удалённый монитор от монитора чужого проекта
		// (ProjectID нулевого Monitor{} не совпадёт) — оба пропускаются одинаково.
		if m.ProjectID != sp.ProjectID {
			continue
		}

		states := statesByMon[m.ID]
		// тот же приоритет, что на странице монитора: пауза → обслуживание → consensus.
		status := monitorStatus(m, states, inMaintenance)

		stat := uptimeByMon[m.ID]
		bars := barsByMon[m.ID]

		// храним BarStats, не готовый SVG: вьюха кешируется и общая для всех —
		// иначе язык прогревшего кеш посетителя увидели бы все на 30с.
		view.Monitors = append(view.Monitors, templates.StatusMonitorView{
			Name:      spm.DisplayName,
			Status:    status,
			Uptime90d: stat,
			BarStats:  bars,
		})

		// монитор на паузе или в обслуживании не портит общий статус — он
		// выведен из-под наблюдения, не «сломан».
		if status == "up" || status == "down" {
			counted++
			if status == "down" {
				down++
			}
		}

		for _, inc := range incidentsByMon[m.ID] {
			if inc.StartedAt.Before(from) {
				continue
			}
			var dur time.Duration
			if inc.ResolvedAt != nil {
				dur = inc.ResolvedAt.Sub(inc.StartedAt)
				if dur < 0 {
					dur = 0 // resolved до started не бывает в норме, но не показываем отрицательное
				}
			}
			incidents = append(incidents, datedIncident{
				view: templates.StatusIncidentView{
					Name:      spm.DisplayName,
					StartedAt: inc.StartedAt.UTC().Format(statusPageTimeLayout),
					Duration:  dur,
					Ongoing:   inc.ResolvedAt == nil,
				},
				at: inc.StartedAt,
			})
		}
	}

	view.Overall = overallStatus(down, counted)

	sort.SliceStable(incidents, func(i, j int) bool { return incidents[i].at.After(incidents[j].at) })
	if len(incidents) > statusPageIncidentsTotal {
		incidents = incidents[:statusPageIncidentsTotal]
	}
	for _, inc := range incidents {
		view.Incidents = append(view.Incidents, inc.view)
	}
	view.Maintenance = upcomingWindows(windows, now, now.Add(statusPageUpcomingWindow))

	return view, nil
}

// время всегда в UTC — часовой пояс проекта внутренняя деталь, JS для
// локализации на странице нет.
const statusPageTimeLayout = "2006-01-02 15:04 UTC"

// страница без учитываемых мониторов (пустая, пауза или обслуживание) считается работающей.
func overallStatus(down, counted int) string {
	switch {
	case counted == 0 || down == 0:
		return "operational"
	case down >= counted:
		return "major"
	default:
		return "partial"
	}
}

// каждое окно разворачивается в интервалы отдельно, чтобы у интервала осталось имя окна.
func upcomingWindows(windows []uptime.Window, from, to time.Time) []templates.StatusWindowView {
	type namedInterval struct {
		name string
		iv   uptime.Interval
	}
	var ivs []namedInterval
	for _, wnd := range windows {
		for _, iv := range uptime.WindowIntervals([]uptime.Window{wnd}, from, to) {
			ivs = append(ivs, namedInterval{name: wnd.Name, iv: iv})
		}
	}
	sort.SliceStable(ivs, func(i, j int) bool { return ivs[i].iv.From.Before(ivs[j].iv.From) })

	out := make([]templates.StatusWindowView, 0, len(ivs))
	for _, ni := range ivs {
		out = append(out, templates.StatusWindowView{
			Name: ni.name,
			From: ni.iv.From.UTC().Format(statusPageTimeLayout),
			To:   ni.iv.To.UTC().Format(statusPageTimeLayout),
		})
	}
	return out
}

// контент страницы — операционная настройка (доступна оператору); публикация
// (slug/enabled) — только owner/admin, гейт в POST-обработчиках.
func (h *Handler) statusPagesPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	h.renderStatusPages(w, r, http.StatusOK, projectID, authz.CanManage, "", nil)
}

// override≠nil подменяет форму (ID==0 — создание, иначе правка); canManage
// приходит от вызывающего гейта, повторно не резолвится.
func (h *Handler) renderStatusPages(w http.ResponseWriter, r *http.Request, status int, projectID int64, canManage bool, errMsg string, override *templates.StatusPageForm) {
	if h.Uptime == nil { // стенд без мониторинга: 404, а не nil-разыменование
		h.notFound(w, r)
		return
	}
	monitors, err := h.Uptime.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	pages, err := h.Uptime.StatusPagesOf(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	// мониторы всех страниц одним запросом, не по запросу на страницу.
	pageIDs := make([]int64, len(pages))
	for i, sp := range pages {
		pageIDs[i] = sp.ID
	}
	selectedByPage, err := h.Uptime.StatusPageMonitorsOf(r.Context(), pageIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	newForm := templates.StatusPageForm{Enabled: true, Monitors: statusPageFormMonitors(monitors, nil)}
	forms := make([]templates.StatusPageForm, 0, len(pages))
	for _, sp := range pages {
		forms = append(forms, templates.StatusPageForm{
			ID:          sp.ID,
			PublicID:    sp.PublicID,
			Title:       sp.Title,
			Description: sp.Description,
			Enabled:     sp.Enabled,
			Monitors:    statusPageFormMonitors(monitors, selectedByPage[sp.ID]),
		})
	}

	if override != nil {
		if override.ID == 0 {
			// иначе модалка (на :target) закроется вместе с заполненными полями.
			newForm = *override
			newForm.Submitted = true
		}
		for i := range forms {
			if forms[i].ID == override.ID {
				forms[i] = *override
			}
		}
	}

	w.WriteHeader(status)
	_ = templates.StatusPagesSettings(projectID, h.BaseURL, forms, newForm, canManage, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

func statusPageFormMonitors(monitors []uptime.Monitor, selected []uptime.StatusPageMonitor) []templates.StatusPageFormMonitor {
	byID := make(map[int64]uptime.StatusPageMonitor, len(selected))
	for _, s := range selected {
		byID[s.MonitorID] = s
	}
	out := make([]templates.StatusPageFormMonitor, 0, len(monitors))
	for _, m := range monitors {
		fm := templates.StatusPageFormMonitor{ID: m.ID, MonitorName: m.Name, DisplayName: m.Name}
		if s, ok := byID[m.ID]; ok {
			fm.Selected = true
			fm.DisplayName = s.DisplayName
		}
		out = append(out, fm)
	}
	return out
}

// принимаются только мониторы этого проекта — id чужого монитора
// игнорируется, иначе страница могла бы показать чужой монитор.
func parseStatusPageForm(r *http.Request, projectID int64, projectMonitors []uptime.Monitor) (uptime.StatusPage, []uptime.StatusPageMonitor) {
	byID := make(map[int64]uptime.Monitor, len(projectMonitors))
	for _, m := range projectMonitors {
		byID[m.ID] = m
	}

	sp := uptime.StatusPage{
		ProjectID:   projectID,
		Title:       strings.TrimSpace(r.FormValue("title")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Enabled:     formBool(r, "enabled"),
	}

	var monitors []uptime.StatusPageMonitor
	for _, raw := range r.Form["monitors"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		m, ok := byID[id]
		if !ok {
			continue
		}
		name := strings.TrimSpace(r.FormValue("display_name_" + raw))
		if name == "" {
			name = m.Name
		}
		monitors = append(monitors, uptime.StatusPageMonitor{
			MonitorID:   id,
			DisplayName: name,
			Position:    len(monitors),
		})
	}
	return sp, monitors
}

// publicID — ключ существующей страницы для ссылки при перерисовке; у формы
// создания вызывающий передаёт "".
func statusPageFormView(id int64, publicID string, sp uptime.StatusPage, monitors []uptime.StatusPageMonitor, projectMonitors []uptime.Monitor) templates.StatusPageForm {
	return templates.StatusPageForm{
		ID:          id,
		PublicID:    publicID,
		Title:       sp.Title,
		Description: sp.Description,
		Enabled:     sp.Enabled,
		Monitors:    statusPageFormMonitors(projectMonitors, monitors),
	}
}

func statusPageErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, uptime.ErrInvalidStatusPage):
		return i18n.T(ctx, "error.statuspage.invalid")
	default:
		return i18n.T(ctx, "error.action_failed")
	}
}

// Enabled — admin-only: оператор без прав получает страницу выключенной,
// что бы ни прислала форма (защита на сервере).
func (h *Handler) statusPagesCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	// лимит per-user до разбора формы и походов в БД — вставка страницы
	// стоит нескольких обращений к PG за попытку.
	if h.statusPageLimiter != nil && !h.statusPageLimiter.Allow("sp-create|"+strconv.FormatInt(uid, 10)) {
		h.renderError(w, r, http.StatusTooManyRequests, i18n.T(r.Context(), "error.statuspage.rate_limited"))
		return
	}
	if !h.parseForm(w, r) {
		return
	}

	if h.Uptime == nil { // стенд без мониторинга: 404, а не nil-разыменование
		h.notFound(w, r)
		return
	}
	projectMonitors, err := h.Uptime.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	sp, monitors := parseStatusPageForm(r, projectID, projectMonitors)

	if !authz.CanManage {
		sp.Enabled = false
	}

	if _, err := h.Uptime.CreateStatusPage(r.Context(), sp, monitors); err != nil {
		if errors.Is(err, uptime.ErrInvalidStatusPage) {
			form := statusPageFormView(0, "", sp, monitors, projectMonitors)
			h.renderStatusPages(w, r, http.StatusUnprocessableEntity, projectID, authz.CanManage, statusPageErrorMessage(r.Context(), err), &form)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, statusPagesPath(projectID), http.StatusSeeOther)
}

// несуществующая страница и страница чужого проекта дают одну и ту же 404 —
// не палим существование чужих id.
func (h *Handler) loadManagedStatusPage(w http.ResponseWriter, r *http.Request, uid int64) (uptime.StatusPage, projectAuthz, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return uptime.StatusPage{}, projectAuthz{}, false
	}
	sp, err := h.Uptime.StatusPageByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "")
			return uptime.StatusPage{}, projectAuthz{}, false
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return uptime.StatusPage{}, projectAuthz{}, false
	}
	authz, ok := h.requireProjectOperator(w, r, sp.ProjectID, uid)
	if !ok {
		return uptime.StatusPage{}, projectAuthz{}, false
	}
	return sp, authz, true
}

// Enabled — admin-only: сервер тихо заменяет присланное значение на текущее из БД.
func (h *Handler) statusPagesUpdate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Uptime == nil { // стенд без мониторинга: 404, а не nil-разыменование
		h.notFound(w, r)
		return
	}
	existing, authz, ok := h.loadManagedStatusPage(w, r, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}

	projectMonitors, err := h.Uptime.List(r.Context(), existing.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	sp, monitors := parseStatusPageForm(r, existing.ProjectID, projectMonitors)
	sp.ID = existing.ID

	if !authz.CanManage {
		sp.Enabled = existing.Enabled
	}

	if err := h.Uptime.UpdateStatusPage(r.Context(), sp, monitors); err != nil {
		if errors.Is(err, uptime.ErrInvalidStatusPage) {
			form := statusPageFormView(sp.ID, existing.PublicID, sp, monitors, projectMonitors)
			h.renderStatusPages(w, r, http.StatusUnprocessableEntity, existing.ProjectID, authz.CanManage, statusPageErrorMessage(r.Context(), err), &form)
			return
		}
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "")
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	// инвалидация по public_id — тому же ключу, по которому резолвится
	// buildStatusPage; он неизменяем, второго значения не бывает.
	h.statusCache.invalidate(existing.PublicID)
	http.Redirect(w, r, statusPagesPath(existing.ProjectID), http.StatusSeeOther)
}

func (h *Handler) statusPagesDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Uptime == nil { // стенд без мониторинга: 404, а не nil-разыменование
		h.notFound(w, r)
		return
	}
	sp, authz, ok := h.loadManagedStatusPage(w, r, uid)
	if !ok {
		return
	}
	// снятие включённой страницы — публикационное решение (нужен admin);
	// невыключенную оператор удаляет как обычный контент.
	if sp.Enabled && !authz.CanManage {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.403.body"))
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	// двухшаговое подтверждение вместо confirm() — CSP без unsafe-inline его не исполняет.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.statuspage_delete.message", "confirm.delete",
			statusPagesPath(sp.ProjectID), "/statuspages/"+strconv.FormatInt(sp.ID, 10)+"/delete", nil,
			"name", sp.Title)
		return
	}
	if err := h.Uptime.DeleteStatusPage(r.Context(), sp.ID); err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "")
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.statusCache.invalidate(sp.PublicID)
	http.Redirect(w, r, statusPagesPath(sp.ProjectID), http.StatusSeeOther)
}

// Перевыпуск публичного адреса — публикационное решение того же уровня, что включение
// страницы: единственный способ отозвать утёкшую ссылку, не потеряв страницу.
func (h *Handler) statusPagesRotate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.Uptime == nil { // стенд без мониторинга: 404, а не nil-разыменование
		h.notFound(w, r)
		return
	}
	sp, authz, ok := h.loadManagedStatusPage(w, r, uid)
	if !ok {
		return
	}
	if !authz.CanManage {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.403.body"))
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	// перевыпуск необратим: прежний адрес перестаёт открываться сразу, восстановить нельзя.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.statuspage_rotate.message", "confirm.statuspage_rotate.action",
			statusPagesPath(sp.ProjectID), "/statuspages/"+strconv.FormatInt(sp.ID, 10)+"/rotate", nil,
			"name", sp.Title)
		return
	}
	if _, err := h.Uptime.RotateStatusPagePublicID(r.Context(), sp.ID); err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "")
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.statusCache.invalidate(sp.PublicID)
	http.Redirect(w, r, statusPagesPath(sp.ProjectID), http.StatusSeeOther)
}
