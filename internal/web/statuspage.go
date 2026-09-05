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

// statusPageBuckets — предельная глубина публичной статус-страницы: 90 суток,
// по корзине на сутки — та же полоска доступности, что и в списке мониторов,
// только суточная. Фактическая глубина не превышает срок хранения результатов
// проверок, см. statusPageDays.
const statusPageBuckets = 90

// statusPageDays — сколько суток показывает публичная статус-страница.
//
// Окно урезается фактическим сроком хранения результатов проверок: он едет на
// той же ручке, что и события (GOTCHA_EVENT_RETENTION_DAYS). Уменьшив её до 14, до
// этой правки оператор молча укорачивал публичную историю до 14 дней — а
// страница по-прежнему рисовала 90 клеток, из которых 76 были пустыми и
// выглядели как «данных нет», то есть как проблема с мониторингом.
//
// 0 в RetentionDays означает «срок не задан» и окно не урезает.
func (h *Handler) statusPageDays() int {
	if h.RetentionDays > 0 && h.RetentionDays < statusPageBuckets {
		return h.RetentionDays
	}
	return statusPageBuckets
}

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
// 30 секунд. Размер карты ограничен: иначе перебор случайных ключей раздул бы
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

// statusBuild — идущая прямо сейчас сборка страницы одного ключа: done
// закрывается, когда view/err заполнены (запись до close, чтение после — этого
// достаточно, отдельного мьютекса на поля не нужно).
type statusBuild struct {
	done chan struct{}
	view templates.StatusPageView
	err  error
}

// statusCache — кеш публичных статус-страниц по ключу (public_id). Нулевое
// значение готово к работе (карты создаются лениво), поэтому Handler держит
// его значением и New не обязан ничего инициализировать.
//
// inflight — single-flight: страницу собирает ровно один запрос на ключ, все
// остальные ждут его результат. Без этого любой аноним, открывший десяток
// параллельных соединений на холодный (или только что протухший) ключ,
// множил бы на десять всю сборку — ~5 запросов в PG и ClickHouse НА КАЖДЫЙ
// монитор страницы, — и так каждые 30 секунд. Роут публичный и без
// аутентификации, так что это самый дешёвый способ разложить бэкенд.
type statusCache struct {
	mu       sync.Mutex
	entries  map[string]statusCacheEntry
	inflight map[string]*statusBuild
}

// load отдаёт живую запись кеша, а на промахе собирает страницу через build —
// но ровно одним вызовом на ключ: пришедшие в этот момент запросы ждут ту же
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
		// TTL отсчитывается от завершения сборки, а не от захода в load: на
		// медленной сборке (до statusPageBuildTimeout) запись, положенная
		// от исходного now, жила бы заметно меньше заявленных 30с.
		c.putLocked(key, b.view, time.Now())
	}
	c.mu.Unlock()

	close(b.done)
	return b.view, b.err
}

// putLocked кладёт готовую страницу в кеш; вызывается под c.mu.
func (c *statusCache) putLocked(key string, view templates.StatusPageView, now time.Time) {
	if c.entries == nil || len(c.entries) >= statusCacheMaxEntries {
		// Переполнение сбрасывает кеш целиком (а не вытесняет одну запись):
		// LRU здесь не нужен — TTL всё равно 30 секунд, а полный сброс не
		// даёт карте расти от перебора случайных ключей.
		c.entries = make(map[string]statusCacheEntry, statusCacheMaxEntries)
	}
	c.entries[key] = statusCacheEntry{view: view, expires: now.Add(statusCacheTTL)}
}

// invalidate выбрасывает ключи (public_id) из кеша: правка или удаление
// страницы в настройках должна быть видна миру сразу, а не через 30 секунд.
// Принимает несколько ключей вариативно (а не один) ради общего вида вызова —
// public_id неизменяем, так что вызывающие передают ровно один.
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

// statusPage — GET /status/{key}: публичная страница, без сессии и без
// какой-либо авторизации (единственный такой браузерный роут — ср.
// heartbeat, машинный). key — непрозрачный public_id страницы; выключенная
// страница и неизвестный ключ дают одинаковую 404: снаружи их не отличить.
//
// Ключ, не резолвящийся напрямую, может оказаться старым slug'ом (до
// перехода на public_id, задача 4 плана): такой резолвится через
// status_page_redirects и уходит 301'ом на актуальный /status/{public_id} —
// старые ссылки/закладки не должны биться. Порядок строго ключ → редирект →
// 404, редирект не кешируется (одиночный дешёвый запрос вне statusCache).
//
// В HTML не попадает ничего внутреннего: только display_name мониторов
// (не имя монитора и не его URL/хост/порт), статус, uptime% и полоска за 90
// дней, инциденты без причины и регионов, ближайшие окна обслуживания.
func (h *Handler) statusPage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	now := time.Now().UTC()

	// Сборку ведёт один запрос на ключ (statusCache.load), остальные ждут его
	// результат. Контекст сборки отвязан от отмены запроса — ждущие не должны
	// получить 500 оттого, что ведущий отвалился на середине (а именно при
	// штурме холодного ключа клиенты и отваливаются), — но ОГРАНИЧЕН по
	// времени, как и в uptime.Runner.runOne: подвисший PG/CH не должен вечно
	// держать соединение пула и горутину ведущего.
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), statusPageBuildTimeout)
	defer cancel()

	view, err := h.statusCache.load(r.Context(), key, now, func() (templates.StatusPageView, error) {
		return h.buildStatusPage(buildCtx, key, now)
	})
	if err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			// Не резолвится как ключ — последний шанс: legacy slug из
			// status_page_redirects. Найден и жив (enabled) → 301 на
			// актуальный ключ; иначе (не найден, или сама страница
			// выключена — StatusPageForRedirect отдаёт found=false и в том,
			// и в другом случае) — обычная 404, не палим разницу.
			pubID, found, rerr := h.Uptime.StatusPageForRedirect(r.Context(), key)
			if rerr != nil {
				if r.Context().Err() != nil {
					// Клиент ушёл, пока мы искали legacy-редирект: писать
					// некому, а context.Canceled — не поломка, логировать
					// как Error и отдавать 500 «в никуда» не за что.
					return
				}
				slog.Error("statusPage: redirect lookup failed", "error", rerr)
				h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
				return
			}
			if found {
				http.Redirect(w, r, "/status/"+pubID, http.StatusMovedPermanently)
				return
			}
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		if r.Context().Err() != nil {
			// Клиент ушёл, пока мы ждали чужую сборку: писать некому.
			return
		}
		// Отказ ClickHouse здесь — СОЗНАТЕЛЬНО 500, а не единый приём
		// деградации CH-страниц (оболочка + «данные временно недоступны»,
		// см. logsList и аудит 2026-09-04, K8-1). Страница публичная и
		// неаутентифицированная, её содержимое целиком из ClickHouse, ответ
		// кешируется (statusCache) и опрашивается внешними поллерами статуса:
		// 200 с заглушкой они прочли бы как «сервис в порядке», а закешированная
		// заглушка пережила бы сам отказ. Честная ошибка здесь информативнее.
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
		// Глубина едет в модель: подписи страницы должны называть то окно,
		// которое реально показано, а оно ограничено сроком хранения.
		Days: days,
	}

	// incidents собираются вместе с исходным временем начала: сортировка идёт
	// по нему, а в модель уходит уже отформатированная строка (чтобы
	// закешированная страница рендерилась байт-в-байт одинаково).
	type datedIncident struct {
		view templates.StatusIncidentView
		at   time.Time
	}

	// Всё, что раньше шло в цикле по мониторам (2N+3N запросов на публичной
	// странице с N мониторами — Get и IncidentsForMonitor по три и один
	// запрос каждый, плюс сам Uptime), теперь читается пакетно, до цикла.
	// Цикл ниже только раскладывает уже прочитанные карты по мониторам и не
	// обращается к БД вообще — именно это и убирает рост, пропорциональный
	// числу мониторов: сорок мониторов и пять окон обслуживания упирались в
	// statusPageBuildTimeout, а посетитель неаутентифицированной страницы
	// получал ошибку.
	spIDs := make([]int64, len(spMonitors))
	for i, spm := range spMonitors {
		spIDs[i] = spm.MonitorID
	}
	statesByMon, err := h.Uptime.StatesBatch(ctx, spIDs)
	if err != nil {
		return templates.StatusPageView{}, err
	}
	// Корзин ровно столько, сколько суток в окне: иначе страница рисует
	// клетки за пределами срока хранения и выдаёт их за «данных нет».
	barsByMon, err := h.UptimeQuery.BarsBatch(ctx, spIDs, from, now, days)
	if err != nil {
		return templates.StatusPageView{}, err
	}
	monitorsByID, err := h.Uptime.GetBatch(ctx, spIDs)
	if err != nil {
		return templates.StatusPageView{}, err
	}
	// exclude — общие окна обслуживания ПРОЕКТА страницы (уже посчитаны выше,
	// до цикла), поэтому один и тот же список годится для всех её мониторов
	// разом, в отличие от per-монитора разных полосок/состояний.
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
		// Монитор чужого проекта на странице не показывается — форма настроек
		// такого не создаст, но подстраховка дешевле утечки. GetBatch
		// заполняет карту для ВСЕХ запрошенных id (см. её докблок): монитора,
		// которого уже нет в БД, здесь не отличить от монитора чужого
		// проекта — ProjectID нулевого Monitor{} никогда не совпадёт с
		// sp.ProjectID, и оба случая одинаково пропускаются, как и раньше
		// делал отдельный errors.Is(err, uptime.ErrNotFound).
		if m.ProjectID != sp.ProjectID {
			continue
		}

		states := statesByMon[m.ID]
		// Тот же приоритет, что и в списке мониторов (пауза → обслуживание →
		// consensus-агрегат uptime.Aggregate) — consensus не дублируем.
		status := monitorStatus(m, states, inMaintenance)

		stat := uptimeByMon[m.ID]
		bars := barsByMon[m.ID]

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
// у каждой существующей. Доступ — оператор проекта (requireProjectOperator):
// контент страницы (title/description/набор мониторов) — операционная
// настройка мониторинга, как окна обслуживания; публикация (slug/enabled)
// остаётся owner/admin-only и защищается ниже, в самих POST-обработчиках
// (спека cld/plans/2026-08-08-access-model-rework.md).
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

// renderStatusPages — общий рендер настроек: GET и все POST на 422. override
// (если не nil) подменяет одну из форм на введённые пользователем значения:
// ID == 0 — форму создания, иначе форму редактирования страницы с этим id.
// canManage приходит от вызывающего (гейт уже посчитал его — находка B4), а
// не резолвится здесь заново отдельным canManageProject.
func (h *Handler) renderStatusPages(w http.ResponseWriter, r *http.Request, status int, projectID int64, canManage bool, errMsg string, override *templates.StatusPageForm) {
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

	// Мониторы всех страниц — одним запросом, а не по запросу на страницу
	// (аудит 2026-09-04, K8-2); порядок — как у StatusPageMonitors.
	pageIDs := make([]int64, len(pages))
	for i, sp := range pages {
		pageIDs[i] = sp.ID
	}
	selectedByPage, err := h.Uptime.StatusPageMonitorsOf(r.Context(), pageIDs)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
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
			// ID==0 — это форма СОЗДАНИЯ: возвращаем введённое и помечаем, что
			// её отправляли, иначе модалка закроется вместе с заполненными
			// полями (она держится на :target, а фрагмента в адресе уже нет).
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
// иначе страница могла бы показать монитор из другого проекта. Публичный
// адрес — сгенерированный public_id; поля slug у StatusPage больше нет (T5).
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

// statusPageFormView — введённые значения формы для перерисовки на 422 (тот
// же набор чекбоксов мониторов проекта, но с отметками и именами из запроса).
// publicID — ключ существующей страницы (для ссылки на публичный адрес на
// повторной отрисовке формы редактирования); у формы создания его ещё нет —
// вызывающий передаёт "".
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

// statusPagesCreate — POST /projects/{id}/statuspages: sameOrigin +
// requireProjectOperator. ErrInvalidStatusPage → 422 с перерисовкой формы и
// сохранением введённых значений. Публикация
// (Enabled) — admin-only: оператор без прав управления получает страницу,
// рождённую выключенной, что бы ни прислала форма (защита на сервере, не на
// видимости чекбокса — спека cld/plans/2026-08-08-access-model-rework.md).
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
	// P2-2: лимит на создание статус-страниц (per-user) — до разбора формы и
	// походов в БД. Раньше повод был ещё и в переборе slug'ов как бесплатном
	// оракуле занятости (успех vs 422 «занято») — с задачи 4 slug форма не
	// шлёт вовсе, этот мотив снят, но сама вставка (несколько походов в PG на
	// попытку) остаётся достаточно дорогой, чтобы её не дать штурмовать без
	// лимита. Легитимный оператор в 12/мин укладывается с запасом.
	if h.statusPageLimiter != nil && !h.statusPageLimiter.Allow("sp-create|"+strconv.FormatInt(uid, 10)) {
		h.renderError(w, r, http.StatusTooManyRequests, i18n.T(r.Context(), "error.statuspage.rate_limited"))
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
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

	if !authz.CanManage {
		// Публикация — уровнем выше (спека 2026-08-08): страница, созданная
		// оператором, рождается выключенной, что бы ни прислала форма.
		sp.Enabled = false
	}

	if _, err := h.Uptime.CreateStatusPage(r.Context(), sp, monitors); err != nil {
		if errors.Is(err, uptime.ErrInvalidStatusPage) {
			form := statusPageFormView(0, "", sp, monitors, projectMonitors)
			h.renderStatusPages(w, r, http.StatusUnprocessableEntity, projectID, authz.CanManage, statusPageErrorMessage(r.Context(), err), &form)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, statusPagesPath(projectID), http.StatusSeeOther)
}

// loadManagedStatusPage — общая часть POST /statuspages/{id} и
// /statuspages/{id}/delete: страница ищется по id, проект берётся из неё
// самой, доступ проверяется в этом проекте по предикату оператора. Несуществующая
// страница и страница чужого проекта дают одну и ту же 404 — не палим
// существование чужих числовых id (тот же принцип, что и в
// loadAccessibleMonitor). Возвращаемый projectAuthz.CanManage используется
// в statusPagesDelete как дополнительный гейт для удаления НЕвыключенной
// страницы — публикационное решение (A3, спека
// cld/plans/2026-08-08-access-model-rework.md); удаление выключенной
// остаётся доступно любому оператору как обычный контент.
func (h *Handler) loadManagedStatusPage(w http.ResponseWriter, r *http.Request, uid int64) (uptime.StatusPage, projectAuthz, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return uptime.StatusPage{}, projectAuthz{}, false
	}
	sp, err := h.Uptime.StatusPageByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return uptime.StatusPage{}, projectAuthz{}, false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return uptime.StatusPage{}, projectAuthz{}, false
	}
	authz, ok := h.requireProjectOperator(w, r, sp.ProjectID, uid)
	if !ok {
		return uptime.StatusPage{}, projectAuthz{}, false
	}
	return sp, authz, true
}

// statusPagesUpdate — POST /statuspages/{id}: sameOrigin + оператор проекта
// самой страницы. 422 перерисовывает именно её форму с введёнными
// значениями. Enabled — admin-only: оператор без прав управления не может
// его сменить, даже прислав другое значение в форме (сервер тихо заменяет
// его на текущее из БД).
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
	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}

	projectMonitors, err := h.Uptime.List(r.Context(), existing.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
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
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Инвалидация — по public_id, не по slug: кеш живёт под ключом, по
	// которому реально резолвится buildStatusPage. public_id неизменяем
	// (T1), поэтому, в отличие от старого slug'а, старого/нового значения
	// тут два не бывает — одного вызова достаточно.
	h.statusCache.invalidate(existing.PublicID)
	http.Redirect(w, r, statusPagesPath(existing.ProjectID), http.StatusSeeOther)
}

// statusPagesDelete — POST /statuspages/{id}/delete.
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
	// Удаление опубликованной страницы снимает её с публичного интернета —
	// это публикационное решение (как Enabled в statusPagesUpdate), а не
	// обычное снятие контента, поэтому оператору недостаточно (спека
	// cld/plans/2026-08-08-access-model-rework.md). Невыключенную страницу
	// оператор по-прежнему удаляет — она ещё никому не видна. Страница уже
	// загружена loadManagedStatusPage, существование не секрет → честный
	// 403, а не 404. CanManage — из того же гейта, что и loadManagedStatusPage
	// (находка B4), второй поход в БД не нужен.
	if sp.Enabled && !authz.CanManage {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.403.body"))
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения вместо необратимого действия.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.statuspage_delete.message", "confirm.delete",
			statusPagesPath(sp.ProjectID), "/statuspages/"+strconv.FormatInt(sp.ID, 10)+"/delete", nil)
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
	h.statusCache.invalidate(sp.PublicID)
	http.Redirect(w, r, statusPagesPath(sp.ProjectID), http.StatusSeeOther)
}
