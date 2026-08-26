package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// sparklineWindow/Buckets — окно и разрешение спарклайнов в списке issues:
// 24 часа, разбитые на 24 часовых корзины.
const (
	sparklineWindow  = 24 * time.Hour
	sparklineBuckets = 24
)

// issuesList — GET /projects/{id}/issues: таблица issues проекта (доступ
// только у CanAccessProject, иначе 404, чтобы не палить существование
// чужих числовых id — тот же принцип, что и в projectSetup).
func (h *Handler) issuesList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
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

	// canManage — виден ли «Project settings» (dead link fix, задача 5/2):
	// ссылка ведёт на страницу, которая сама требует owner/admin, поэтому
	// member её видеть не должен вовсе. org.ErrNotMember не должен ронять
	// страницу — это лишь означает, что показывать ссылку не нужно (доступ к
	// проекту у юзера мог быть только через команду).
	orgID, err := h.Org.ProjectOrg(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	role, err := h.Org.Role(r.Context(), orgID, uid)
	if err != nil && !errors.Is(err, org.ErrNotMember) {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	canManage := role == org.RoleOwner || role == org.RoleAdmin

	q := r.URL.Query()
	// Дефолтный вид списка — только «Не решено»: решённые и игнорируемые
	// проблемы обычно не нужны при первом открытии и лишь зашумляют список.
	// Отличаем «параметр не задан» (первый заход → unresolved) от «пользователь
	// явно выбрал Все» (?status= присутствует, но пусто → без фильтра): так
	// дефолт остаётся полностью переопределяемым из UI-фильтра.
	status := q.Get("status")
	if !q.Has("status") {
		status = "unresolved"
	}
	// Тот же контрол окна времени, что и на остальных страницах, с
	// дополнительным пунктом «за всё время» — он же по умолчанию: большинство
	// групп старше суток, и окно на 24 часа показывало бы пустой список на
	// здоровом проекте.
	rng := h.resolveTimeRange(w, r, RangeAll)
	filter := issue.Filter{
		Status:      status,
		Level:       q.Get("level"),
		Query:       q.Get("q"),
		Sort:        q.Get("sort"),
		Environment: q.Get("env"),
		Since:       rng.From,
		Until:       rng.To,
		Page:        parsePage(q.Get("page")),
	}

	items, total, err := h.Issues.List(r.Context(), projectID, filter)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	environments, err := h.Issues.Environments(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	sparklines, err := h.sparklinesFor(r.Context(), projectID, items)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	rows := make([]templates.IssueRow, len(items))
	for i, it := range items {
		rows[i] = templates.IssueRow{
			Issue: it,
			// Тренд событий за сутки: значения — счётчики, поэтому в
			// подсказке показываются как есть.
			Sparkline: sparklineSVG(r.Context(), sparklines[it.ID], sparklineWidth, sparklineHeight, nil),
		}
	}

	page := filter.Page
	if page < 1 {
		page = 1
	}
	rangeVM := timeRangeVM(rng)
	rangeVM.AllowAll = true
	tplFilter := templates.IssuesFilter{
		Status:      filter.Status,
		Level:       filter.Level,
		Query:       filter.Query,
		Sort:        filter.Sort,
		Environment: filter.Environment,
		Range:       rangeVM,
		// №23: фильтр «активен», если сужает список относительно умолчаний
		// (unresolved + всё время). Явный ?status= («Все») и явный
		// ?status=unresolved список не сужают — это не повод показывать
		// «ничего не подошло» вместо «событий ещё не было».
		Active: filter.Level != "" || filter.Query != "" || filter.Environment != "" ||
			(q.Has("status") && status != "" && status != "unresolved") ||
			rng.Key != RangeAll,
	}
	banner := h.quotaBanner(r.Context(), orgID, canManage)
	// canAccess здесь эквивалентен canOperateProject (C5): последний сегодня
	// буквально вызывает CanAccessProject (см. operate.go), а доступ к
	// странице issues уже подтверждён им выше — второй поход в БД не нужен.
	gs := h.gettingStarted(r.Context(), uid, projectID, orgID, canManage, canAccess)
	// canManage больше не передаётся (№117): шаблон его не использовал —
	// роль питает QuotaBanner и GettingStartedVM выше.
	//
	// canAccess как canOperate (E1, задача 11): гейтит кнопки экспорта на
	// списке — тот же predicate, что canOperateProject использует для
	// requireProjectOperator (см. её докблок в operate.go и комментарий выше
	// про canAccess/gettingStarted). Дополнительно — h.Exports != nil: на
	// инстансе без каталога выгрузок воркер не стартует, кнопка «Выгрузить»
	// поведёт на 404 (ревью веб-части E1, п.3).
	canExport := canAccess && h.Exports != nil
	_ = templates.IssuesList(projectID, rows, tplFilter, page, total, h.currentEmail(r), environments, banner, gs, canExport).Render(r.Context(), w)
}

// gettingStarted собирает вьюмодель чек-листа «Первые шаги» (задача 5,
// docs-onboarding): шаг 1 (создать проект) всегда закрыт — раз страница
// issues открылась, проект уже существует. Остальные шаги определяются по
// реальным данным: подключён ли SDK (есть хотя бы одна issue — фильтр
// пустой, а не тот, что пришёл в query, иначе активный ?status=resolved
// без открытых issues спрятал бы уже закрытый шаг), настроено ли хотя бы
// одно оповещение, позвана ли команда (>1 участника в орге) или добавлен ли
// хотя бы один монитор аптайма.
//
// Как и quotaBanner, это вспомогательная (не критичная) вьюмодель: сервисы
// могут быть не подключены к стенду (h.Alerts/h.Uptime == nil в части
// тестовых стендов), а сами запросы — упасть с ошибкой сети/БД. Ни то, ни
// другое не должно ронять страницу issues 500-й — недостающий сигнал просто
// трактуется как «шаг ещё не закрыт», а причина логируется на Warn.
func (h *Handler) gettingStarted(ctx context.Context, uid, projectID, orgID int64, canManage, canOperate bool) templates.GettingStartedVM {
	gs := templates.GettingStartedVM{ProjectID: projectID, OrgID: orgID, CanManage: canManage, CanOperate: canOperate}

	// №71: скрытый пользователем чек-лист не считается и не рендерится —
	// пустая VM с CanManage=false никогда не пройдёт условие показа.
	if hidden, err := h.Auth.HideGettingStarted(ctx, uid); err != nil {
		slog.Warn("gettingStarted: hide flag lookup failed", "user_id", uid, "err", err)
	} else if hidden {
		return templates.GettingStartedVM{}
	}

	if exists, err := h.Issues.Exists(ctx, projectID); err != nil {
		slog.Warn("gettingStarted: issues exists check failed", "project_id", projectID, "err", err)
	} else {
		gs.Step2Done = exists
	}

	// B1: Channels() здесь вызывается напрямую, мимо channelsForView —
	// намеренно (allowlist-запись "count-only" в internal/guards). Результат
	// идёт только в len(channels) ниже, ни Target, ни Secret никогда не
	// покидают эту функцию — маскировать нечего, а гонять через дверь ради
	// счётчика было бы лишним чтением.
	if h.Alerts == nil {
		slog.Warn("gettingStarted: Alerts service not configured", "project_id", projectID)
	} else if channels, err := h.Alerts.Channels(ctx, projectID); err != nil {
		slog.Warn("gettingStarted: alert channels failed", "project_id", projectID, "err", err)
	} else {
		gs.Step3Done = len(channels) > 0
	}

	// №71: команда и монитор — два отдельных шага с раздельными ссылками,
	// а не один «или/или» с единственной дверью в настройки организации.
	if members, err := h.Org.MembersOf(ctx, orgID); err != nil {
		slog.Warn("gettingStarted: org members failed", "org_id", orgID, "err", err)
	} else {
		gs.Step4aDone = len(members) > 1
	}
	if h.Uptime == nil {
		slog.Warn("gettingStarted: Uptime service not configured", "project_id", projectID)
	} else if monitors, err := h.Uptime.List(ctx, projectID); err != nil {
		slog.Warn("gettingStarted: uptime monitors failed", "project_id", projectID, "err", err)
	} else {
		gs.Step4bDone = len(monitors) > 0
	}

	gs.Done = 1
	for _, done := range []bool{gs.Step2Done, gs.Step3Done, gs.Step4aDone, gs.Step4bDone} {
		if done {
			gs.Done++
		}
	}
	return gs
}

// gettingStartedHide — POST /profile/getting-started/hide (№71): скрывает
// чек-лист «Первые шаги» навсегда для текущего пользователя. Flash не нужен
// — карточка просто исчезает со страницы, на которую ведёт редирект.
func (h *Handler) gettingStartedHide(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := h.Auth.SetHideGettingStarted(r.Context(), uid); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, safeRedirect(r, h.BaseURL), http.StatusSeeOther)
}

// sparklinesFor — один запрос Events.Sparklines на все issues страницы
// (а не по запросу на issue), как того требует спека.
func (h *Handler) sparklinesFor(ctx context.Context, projectID int64, items []issue.Issue) (map[int64][]uint64, error) {
	if len(items) == 0 {
		return map[int64][]uint64{}, nil
	}
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	since := time.Now().Add(-sparklineWindow)
	return h.Events.Sparklines(ctx, projectID, ids, since, sparklineBuckets)
}

// maxPage — верхняя граница номера страницы. Без неё `?page=<огромное>` даёт
// (page-1)*perPage с переполнением int → отрицательный SQL OFFSET → 500 на
// списках проблем/инцидентов/детали монитора. Потолок заведомо выше любого
// реального набора (при perPage≤100 это ≥100M строк), поэтому легитимную
// глубокую пагинацию не ограничивает, а перелившийся ввод даёт пустую страницу.
const maxPage = 1_000_000

func parsePage(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 1
	}
	if n > maxPage {
		return maxPage
	}
	return n
}

// bulkActionStatus — whitelist допустимых POST-действий bulk-панели.
var bulkActionStatus = map[string]string{
	"resolve":   "resolved",
	"ignore":    "ignored",
	"unresolve": "unresolved",
}

// bulkActionFlashKey — сообщение об итоге каждого массового действия.
var bulkActionFlashKey = map[string]string{
	"resolve":   "flash.issues_resolved",
	"ignore":    "flash.issues_ignored",
	"unresolve": "flash.issues_reopened",
}

// BulkActionFlashKeys — экспортированный набор значений bulkActionFlashKey,
// для internal/guards: ключ здесь собирается не литералом в вызове flashOK
// (issuesBulk передаёт bulkActionFlashKey[r.FormValue("action")]), поэтому
// сканер по местам вызова (internal/guards/flash_test.go) его не видит.
// Экспорт значений — тот же приём, каким TestDynamicKeysResolve
// (internal/guards/i18n_dynamic_test.go) читает uptime.Kinds/org.Platforms:
// множество значений знает только код-владелец, и правило обязано читать
// его оттуда, а не копировать литералом в тест, где копия неизбежно
// разойдётся с картой при следующей правке issuesBulk.
//
// Строится из bulkActionFlashKey, а не дублирует её литералы — единственный
// источник истины остаётся один.
var BulkActionFlashKeys = func() []string {
	out := make([]string, 0, len(bulkActionFlashKey))
	for _, key := range bulkActionFlashKey {
		out = append(out, key)
	}
	return out
}()

// issuesBulk — POST /projects/{id}/issues/bulk: action=resolve|ignore|unresolve
// + ids[] → SetStatusBulk → 303. Редирект идёт на Referer (сохраняет текущие
// фильтры/страницу), если Referer same-origin, иначе на список issues без
// query — тот же принцип sameOrigin, что и у остальных POST в этом пакете.
func (h *Handler) issuesBulk(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
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

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	status, ok := bulkActionStatus[r.FormValue("action")]
	if !ok {
		http.Error(w, "bad action", http.StatusBadRequest)
		return
	}
	ids := parseIDs(r.Form["ids"])
	if len(ids) == 0 {
		// Раньше пустой выбор давал полную перезагрузку и ровно ничего: человек
		// не мог отличить «не отметил» от «не сработало».
		h.flashWarn(w, "flash.nothing_selected", 0)
		http.Redirect(w, r, BulkRedirectTarget(r, h.BaseURL, projectID), http.StatusSeeOther)
		return
	}
	n, err := h.Issues.SetStatusBulk(r.Context(), projectID, ids, status)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Сообщаем ЧИСЛО и действие: строка просто исчезала из списка, и понять,
	// сработало ли и на скольких, было нельзя.
	h.flashOK(w, bulkActionFlashKey[r.FormValue("action")], int(n))

	http.Redirect(w, r, BulkRedirectTarget(r, h.BaseURL, projectID), http.StatusSeeOther)
}

func parseIDs(raw []string) []int64 {
	ids := make([]int64, 0, len(raw))
	for _, s := range raw {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, n)
	}
	return ids
}

// BulkRedirectTarget — Referer, если он same-origin (сохраняет query string
// со всеми фильтрами и текущей страницей пагинации), иначе список issues
// проекта без query. Отвергает пути, начинающиеся с "//" (protocol-relative
// URLs) или "/\" (браузеры при навигации нормализуют обратный слэш в прямой,
// так что "/\evil.com" превращается в "//evil.com" — тот же protocol-relative
// обход), чтобы предотвратить открытые редиректы.
func BulkRedirectTarget(r *http.Request, baseURL string, projectID int64) string {
	ref := r.Header.Get("Referer")
	if ref != "" && isSameOriginURL(ref, baseURL) {
		if u, err := url.Parse(ref); err == nil {
			if !isLocalPath(u.Path) {
				return projectIssuesPath(projectID)
			}
			return u.RequestURI()
		}
	}
	return projectIssuesPath(projectID)
}
