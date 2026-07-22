package web

import (
	"context"
	"errors"
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

// statusPageWindow/Buckets — окно и разрешение публичной статус-страницы: 90
// дней, 90 корзин (одна на сутки) — та же полоска доступности, что и в списке
// мониторов, только суточная.
const (
	statusPageWindow  = 90 * 24 * time.Hour
	statusPageBuckets = 90
)

// statusPageUpcomingWindow — насколько вперёд статус-страница показывает
// окна обслуживания.
const statusPageUpcomingWindow = 7 * 24 * time.Hour

// statusPageIncidentsPerMonitor/Total — сколько инцидентов запрашивается по
// каждому монитору страницы и сколько (самых свежих) в итоге показывается.
const (
	statusPageIncidentsPerMonitor = 50
	statusPageIncidentsTotal      = 20
)

// statusCacheTTL/statusCacheMaxEntries — публичная страница отдаётся анониму,
// поэтому каждый её рендер (десятки запросов в PG и ClickHouse) кешируется на
// 30 секунд. Размер карты ограничен: иначе перебор случайных slug'ов раздул бы
// память. Кешируются только успешные страницы — 404 не кешируется, иначе
// только что включённая страница была бы недоступна до истечения TTL.
const (
	statusCacheTTL        = 30 * time.Second
	statusCacheMaxEntries = 100
)

// statusPageBuildTimeout — потолок на одну сборку страницы. Сборка переживает
// отмену запроса-ведущего (её ждут другие), поэтому единственное, что вообще
// может её завершить при недоступном PG/CH, — этот таймаут.
const statusPageBuildTimeout = 10 * time.Second

// statusCacheEntry — готовая модель страницы и момент, после которого она
// протухла.
type statusCacheEntry struct {
	view    templates.StatusPageView
	expires time.Time
}

// statusBuild — идущая прямо сейчас сборка страницы одного slug'а: done
// закрывается, когда view/err заполнены (запись до close, чтение после — этого
// достаточно, отдельного мьютекса на поля не нужно).
type statusBuild struct {
	done chan struct{}
	view templates.StatusPageView
	err  error
}

// statusCache — кеш публичных статус-страниц по slug'у. Нулевое значение
// готово к работе (карты создаются лениво), поэтому Handler держит его
// значением и New не обязан ничего инициализировать.
//
// inflight — single-flight: страницу собирает ровно один запрос на slug, все
// остальные ждут его результат. Без этого любой аноним, открывший десяток
// параллельных соединений на холодный (или только что протухший) slug,
// множил бы на десять всю сборку — ~5 запросов в PG и ClickHouse НА КАЖДЫЙ
// монитор страницы, — и так каждые 30 секунд. Роут публичный и без
// аутентификации, так что это самый дешёвый способ разложить бэкенд.
type statusCache struct {
	mu       sync.Mutex
	entries  map[string]statusCacheEntry
	inflight map[string]*statusBuild
}

// load отдаёт живую запись кеша, а на промахе собирает страницу через build —
// но ровно одним вызовом на slug: пришедшие в этот момент запросы ждут ту же
// сборку и делят её результат. Кешируется только успех: 404 (ErrNotFound)
// возвращается всем ждущим, но в кеш не попадает — только что включённая
// страница должна быть видна сразу.
//
// ctx — контекст ЖДУЩЕГО (запроса, а не сборки): отвалившийся клиент не должен
// оставлять после себя горутину, залипшую на <-b.done. Роут публичный, без
// аутентификации и без rate limit'а, так что подвисший PG/CH иначе означал бы
// горутину на каждый запрос — навсегда. Ведущий сборку не бросает (её результат
// ждут другие), но и она не вечная: её контекст ограничен таймаутом (см.
// statusPage).
func (c *statusCache) load(ctx context.Context, slug string, now time.Time, build func() (templates.StatusPageView, error)) (templates.StatusPageView, error) {
	c.mu.Lock()
	if e, ok := c.entries[slug]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.view, nil
	}
	if b, ok := c.inflight[slug]; ok {
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
	c.inflight[slug] = b
	c.mu.Unlock()

	b.view, b.err = build()

	c.mu.Lock()
	delete(c.inflight, slug)
	if b.err == nil {
		c.putLocked(slug, b.view, now)
	}
	c.mu.Unlock()

	close(b.done)
	return b.view, b.err
}

// putLocked кладёт готовую страницу в кеш; вызывается под c.mu.
func (c *statusCache) putLocked(slug string, view templates.StatusPageView, now time.Time) {
	if c.entries == nil || len(c.entries) >= statusCacheMaxEntries {
		// Переполнение сбрасывает кеш целиком (а не вытесняет одну запись):
		// LRU здесь не нужен — TTL всё равно 30 секунд, а полный сброс не
		// даёт карте расти от перебора случайных slug'ов.
		c.entries = make(map[string]statusCacheEntry, statusCacheMaxEntries)
	}
	c.entries[slug] = statusCacheEntry{view: view, expires: now.Add(statusCacheTTL)}
}

// invalidate выбрасывает slug'и из кеша: правка или удаление страницы в
// настройках должна быть видна миру сразу, а не через 30 секунд (у правки
// slug'ов два — старый и новый).
func (c *statusCache) invalidate(slugs ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, slug := range slugs {
		delete(c.entries, slug)
	}
}

func statusPagesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/statuspages"
}

// statusPage — GET /status/{slug}: публичная страница, без сессии и без
// какой-либо авторизации (единственный такой браузерный роут — ср.
// heartbeat, машинный). Выключенная страница и неизвестный slug дают
// одинаковую 404: снаружи их не отличить.
//
// В HTML не попадает ничего внутреннего: только display_name мониторов
// (не имя монитора и не его URL/хост/порт), статус, uptime% и полоска за 90
// дней, инциденты без причины и регионов, ближайшие окна обслуживания.
func (h *Handler) statusPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	now := time.Now().UTC()

	// Сборку ведёт один запрос на slug (statusCache.load), остальные ждут его
	// результат. Контекст сборки отвязан от отмены запроса — ждущие не должны
	// получить 500 оттого, что ведущий отвалился на середине (а именно при
	// штурме холодного slug'а клиенты и отваливаются), — но ОГРАНИЧЕН по
	// времени, как и в uptime.Runner.runOne: подвисший PG/CH не должен вечно
	// держать соединение пула и горутину ведущего.
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), statusPageBuildTimeout)
	defer cancel()

	view, err := h.statusCache.load(r.Context(), slug, now, func() (templates.StatusPageView, error) {
		return h.buildStatusPage(buildCtx, slug, now)
	})
	if err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		if r.Context().Err() != nil {
			// Клиент ушёл, пока мы ждали чужую сборку: писать некому.
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.renderStatusPage(w, r, view)
}

// renderStatusPage достраивает локалезависимую часть вьюхи под конкретный
// запрос. Кешированная view общая для всех посетителей, поэтому её нельзя
// мутировать — Monitors копируется, и SVG-полоска строится в копии.
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

// buildStatusPage собирает модель публичной страницы: статусы мониторов
// (uptime.Aggregate — та же consensus-политика, что у детектора), uptime% и
// полоска за 90 дней (окна обслуживания исключены из знаменателя, как на
// странице монитора), инциденты за 90 дней и ближайшие окна обслуживания.
func (h *Handler) buildStatusPage(ctx context.Context, slug string, now time.Time) (templates.StatusPageView, error) {
	sp, spMonitors, err := h.Uptime.StatusPageBySlug(ctx, slug)
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

	from := now.Add(-statusPageWindow)
	exclude := uptime.WindowIntervals(windows, from, now)

	view := templates.StatusPageView{
		Title:       sp.Title,
		Description: sp.Description,
	}

	// incidents собираются вместе с исходным временем начала: сортировка идёт
	// по нему, а в модель уходит уже отформатированная строка (чтобы
	// закешированная страница рендерилась байт-в-байт одинаково).
	type datedIncident struct {
		view templates.StatusIncidentView
		at   time.Time
	}

	var down, counted int
	var incidents []datedIncident
	for _, spm := range spMonitors {
		m, err := h.Uptime.Get(ctx, spm.MonitorID)
		if err != nil {
			if errors.Is(err, uptime.ErrNotFound) {
				continue
			}
			return templates.StatusPageView{}, err
		}
		// Монитор чужого проекта на странице не показывается — форма настроек
		// такого не создаст, но подстраховка дешевле утечки.
		if m.ProjectID != sp.ProjectID {
			continue
		}

		states, err := h.Uptime.States(ctx, m.ID)
		if err != nil {
			return templates.StatusPageView{}, err
		}
		// Тот же приоритет, что и в списке мониторов (пауза → обслуживание →
		// consensus-агрегат uptime.Aggregate) — consensus не дублируем.
		status := monitorStatus(m, states, inMaintenance)

		stat, err := h.UptimeQuery.Uptime(ctx, m.ID, from, now, exclude)
		if err != nil {
			return templates.StatusPageView{}, err
		}
		bars, err := h.UptimeQuery.Bars(ctx, m.ID, from, now, statusPageBuckets)
		if err != nil {
			return templates.StatusPageView{}, err
		}

		// BarStats, а не готовый SVG: вьюха кешируется на statusPageTTL и
		// общая для всех посетителей, а подписи <title> внутри полоски
		// локализованы. Отрендерить SVG здесь — значит впечатать язык того,
		// кто первым прогрел кеш, всем остальным на 30 секунд.
		view.Monitors = append(view.Monitors, templates.StatusMonitorView{
			Name:      spm.DisplayName,
			Status:    status,
			Uptime90d: stat,
			BarStats:  bars,
		})

		// Монитор в окне обслуживания и на паузе не портит общий статус: он
		// не «сломан», он выведен из-под наблюдения.
		if status == "up" || status == "down" {
			counted++
			if status == "down" {
				down++
			}
		}

		monIncidents, err := h.Uptime.IncidentsForMonitor(ctx, m.ID, statusPageIncidentsPerMonitor)
		if err != nil {
			return templates.StatusPageView{}, err
		}
		for _, inc := range monIncidents {
			if inc.StartedAt.Before(from) {
				continue
			}
			incidents = append(incidents, datedIncident{
				view: templates.StatusIncidentView{
					Name:      spm.DisplayName,
					StartedAt: inc.StartedAt.UTC().Format(statusPageTimeLayout),
					Duration:  incidentDurationText(inc, now),
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

// statusPageTimeLayout — время на публичной странице всегда в UTC: часовой
// пояс проекта — тоже внутренняя деталь, а JS для локализации на странице
// нет.
const statusPageTimeLayout = "2006-01-02 15:04 UTC"

// overallStatus — общий статус страницы по числу мониторов в down среди тех,
// чей статус вообще участвует в подсчёте (counted): ни одного — «All systems
// operational», часть — «Partial outage», все — «Major outage». Страница без
// таких мониторов (пустая, вся на паузе или вся в обслуживании) считается
// работающей.
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

// incidentDurationText — длительность закрытого инцидента. Для незакрытого
// возвращает пустую строку: слово «идёт» локализовано и подставляется
// шаблоном по флагу StatusIncidentView.Ongoing, потому что вьюха кешируется
// одна на всех посетителей независимо от их языка. Причина и регионы
// инцидента наружу не отдаются (в них хосты/IP), только имя сервиса, начало
// и длительность.
func incidentDurationText(inc uptime.Incident, now time.Time) string {
	end := now
	if inc.ResolvedAt != nil {
		end = *inc.ResolvedAt
	} else {
		return ""
	}
	d := end.Sub(inc.StartedAt)
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if h > 0 {
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	}
	return strconv.Itoa(m) + "m"
}

// upcomingWindows — окна обслуживания, пересекающие [from,to): каждое окно
// разворачивается в конкретные интервалы (uptime.WindowIntervals) отдельно,
// чтобы у интервала осталось имя окна.
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

// --- Настройки статус-страниц проекта -------------------------------------

// statusPagesPage — GET /projects/{id}/statuspages: список статус-страниц
// проекта со ссылками на публичный URL, форма создания и форма редактирования
// у каждой существующей. Доступ — owner/admin организации проекта
// (requireProjectRole), как у окон обслуживания: страница меняет то, что
// видит мир.
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
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	h.renderStatusPages(w, r, http.StatusOK, projectID, "", nil)
}

// renderStatusPages — общий рендер настроек: GET и все POST на 422. override
// (если не nil) подменяет одну из форм на введённые пользователем значения:
// ID == 0 — форму создания, иначе форму редактирования страницы с этим id.
func (h *Handler) renderStatusPages(w http.ResponseWriter, r *http.Request, status int, projectID int64, errMsg string, override *templates.StatusPageForm) {
	if h.Uptime == nil { // стенд без мониторинга: 404, а не nil-разыменование
		h.notFound(w, r)
		return
	}
	monitors, err := h.Uptime.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	pages, err := h.Uptime.StatusPagesOf(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	newForm := templates.StatusPageForm{Enabled: true, Monitors: statusPageFormMonitors(monitors, nil)}
	forms := make([]templates.StatusPageForm, 0, len(pages))
	for _, sp := range pages {
		selected, err := h.Uptime.StatusPageMonitors(r.Context(), sp.ID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		forms = append(forms, templates.StatusPageForm{
			ID:          sp.ID,
			Slug:        sp.Slug,
			Title:       sp.Title,
			Description: sp.Description,
			Enabled:     sp.Enabled,
			Monitors:    statusPageFormMonitors(monitors, selected),
		})
	}

	if override != nil {
		if override.ID == 0 {
			newForm = *override
		}
		for i := range forms {
			if forms[i].ID == override.ID {
				forms[i] = *override
			}
		}
	}

	w.WriteHeader(status)
	_ = templates.StatusPagesSettings(projectID, h.BaseURL, forms, newForm, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// statusPageFormMonitors — чекбоксы всех мониторов проекта: отмеченные (и с
// заданным display_name) — те, что уже на странице; у остальных display_name
// по умолчанию равен имени монитора.
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

// parseStatusPageForm собирает StatusPage и список её мониторов из формы.
// Позиции — порядок чекбоксов в форме (браузер шлёт их в порядке DOM).
// Принимаются только мониторы этого проекта: id чужого монитора игнорируется,
// иначе страница могла бы показать монитор из другого проекта.
func parseStatusPageForm(r *http.Request, projectID int64, projectMonitors []uptime.Monitor) (uptime.StatusPage, []uptime.StatusPageMonitor) {
	byID := make(map[int64]uptime.Monitor, len(projectMonitors))
	for _, m := range projectMonitors {
		byID[m.ID] = m
	}

	sp := uptime.StatusPage{
		ProjectID:   projectID,
		Slug:        strings.TrimSpace(r.FormValue("slug")),
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

// statusPageFormView — введённые значения формы для перерисовки на 422 (тот
// же набор чекбоксов мониторов проекта, но с отметками и именами из запроса).
func statusPageFormView(id int64, sp uptime.StatusPage, monitors []uptime.StatusPageMonitor, projectMonitors []uptime.Monitor) templates.StatusPageForm {
	return templates.StatusPageForm{
		ID:          id,
		Slug:        sp.Slug,
		Title:       sp.Title,
		Description: sp.Description,
		Enabled:     sp.Enabled,
		Monitors:    statusPageFormMonitors(projectMonitors, monitors),
	}
}

func statusPageErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, uptime.ErrSlugTaken):
		return i18n.T(ctx, "error.slug.taken")
	case errors.Is(err, uptime.ErrInvalidStatusPage):
		return i18n.T(ctx, "error.statuspage.invalid")
	default:
		return i18n.T(ctx, "error.action_failed")
	}
}

// statusPagesCreate — POST /projects/{id}/statuspages: sameOrigin +
// requireProjectRole. ErrInvalidStatusPage/ErrSlugTaken → 422 с
// перерисовкой формы и сохранением введённых значений.
func (h *Handler) statusPagesCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	if h.Uptime == nil { // стенд без мониторинга: 404, а не nil-разыменование
		h.notFound(w, r)
		return
	}
	projectMonitors, err := h.Uptime.List(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	sp, monitors := parseStatusPageForm(r, projectID, projectMonitors)

	if _, err := h.Uptime.CreateStatusPage(r.Context(), sp, monitors); err != nil {
		if errors.Is(err, uptime.ErrInvalidStatusPage) || errors.Is(err, uptime.ErrSlugTaken) {
			form := statusPageFormView(0, sp, monitors, projectMonitors)
			h.renderStatusPages(w, r, http.StatusUnprocessableEntity, projectID, statusPageErrorMessage(r.Context(), err), &form)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, statusPagesPath(projectID), http.StatusSeeOther)
}

// loadManagedStatusPage — общая часть POST /statuspages/{id} и
// /statuspages/{id}/delete: страница ищется по id, проект берётся из неё
// самой, роль проверяется в этом проекте. Несуществующая страница и страница
// чужого проекта дают одну и ту же 404 — не палим существование чужих
// числовых id (тот же принцип, что и в loadAccessibleMonitor).
func (h *Handler) loadManagedStatusPage(w http.ResponseWriter, r *http.Request, uid int64) (uptime.StatusPage, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return uptime.StatusPage{}, false
	}
	sp, err := h.Uptime.StatusPageByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return uptime.StatusPage{}, false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return uptime.StatusPage{}, false
	}
	if _, ok := h.requireProjectRole(w, r, sp.ProjectID, uid); !ok {
		return uptime.StatusPage{}, false
	}
	return sp, true
}

// statusPagesUpdate — POST /statuspages/{id}: sameOrigin + роль в проекте
// самой страницы. 422 перерисовывает именно её форму с введёнными значениями.
func (h *Handler) statusPagesUpdate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	existing, ok := h.loadManagedStatusPage(w, r, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	projectMonitors, err := h.Uptime.List(r.Context(), existing.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	sp, monitors := parseStatusPageForm(r, existing.ProjectID, projectMonitors)
	sp.ID = existing.ID

	if err := h.Uptime.UpdateStatusPage(r.Context(), sp, monitors); err != nil {
		if errors.Is(err, uptime.ErrInvalidStatusPage) || errors.Is(err, uptime.ErrSlugTaken) {
			form := statusPageFormView(sp.ID, sp, monitors, projectMonitors)
			h.renderStatusPages(w, r, http.StatusUnprocessableEntity, existing.ProjectID, statusPageErrorMessage(r.Context(), err), &form)
			return
		}
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.statusCache.invalidate(existing.Slug, sp.Slug)
	http.Redirect(w, r, statusPagesPath(existing.ProjectID), http.StatusSeeOther)
}

// statusPagesDelete — POST /statuspages/{id}/delete.
func (h *Handler) statusPagesDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	sp, ok := h.loadManagedStatusPage(w, r, uid)
	if !ok {
		return
	}
	if err := h.Uptime.DeleteStatusPage(r.Context(), sp.ID); err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.statusCache.invalidate(sp.Slug)
	http.Redirect(w, r, statusPagesPath(sp.ProjectID), http.StatusSeeOther)
}
