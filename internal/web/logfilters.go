package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// logFilterParams — закрытый список параметров отбора логов, которые
// хендлеры сохранённых фильтров читают из POST-формы. Тот же набор полей,
// что разбирает parseLogFilter из query (без before/tskip — курсор
// пагинации не переносится в сохранённый фильтр и не должен переживать
// сохранение/применение). Список закрытый и используется ОБЕИМИ операциями
// — сборкой предикатов для Store.Create/Update и адресом возврата после
// успеха: произвольное поле формы (вроде "back") в адрес редиректа не
// попадает никогда, что и закрывает открытый редирект (по нему у проекта
// уже был алерт безопасности).
var logFilterParams = []string{
	"severity", "service", "environment", "q", "attr", "trace_id",
	"q_not", "severity_not", "service_not", "environment_not", "attr_not",
}

// logFilterFormParams извлекает параметры отбора из уже разобранной
// POST-формы (h.parseForm должен быть вызван раньше) по закрытому списку
// logFilterParams. Тем же значением строятся и предикаты для сохранения
// (см. logFilterPredicatesFromForm), и адрес возврата после успеха —
// расхождение между «что сохранили» и «куда вернулись» невозможно по
// построению: оба читают одну и ту же форму одним и тем же списком имён.
func logFilterFormParams(r *http.Request) url.Values {
	q := url.Values{}
	for _, name := range logFilterParams {
		if vs, ok := r.PostForm[name]; ok {
			q[name] = vs
		}
	}
	return q
}

// logFilterPredicatesFromForm собирает предикаты из параметров отбора формы
// (logFilterFormParams), переиспользуя разбор query-параметров списка логов
// (parseLogFilter) и обратное свёртывание в предикаты (filterToPredicates,
// задача 5/9) — та же пара функций, что применяет сохранённый фильтр
// (applyPredicates) в обратную сторону. TimeRange{} и retentionDays=0 здесь
// не участвуют в результате: filterToPredicates не читает From/To вовсе,
// сохранённый фильтр не несёт временное окно (logfilter.Filter.Predicates).
func logFilterPredicatesFromForm(r *http.Request) []log.Predicate {
	f, _ := parseLogFilter(logFilterFormParams(r), TimeRange{}, 0)
	return filterToPredicates(f)
}

// logFiltersGate — общая часть входа во все четыре хендлера управления
// сохранёнными фильтрами: чужой Origin, отсутствие сессии, {id} вне проекта,
// стенд без проводки стора, отсутствие доступа к проекту (lvlAccess — та же
// граница, что у самого списка логов) и разбор тела формы. Возвращает
// ok=false, если сама проверка уже отправила ответ — вызывающему остаётся
// просто вернуться.
func (h *Handler) logFiltersGate(w http.ResponseWriter, r *http.Request) (projectID, uid int64, ok bool) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
		return 0, 0, false
	}
	uid, authOK := auth.UserID(r.Context())
	if !authOK {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return 0, 0, false
	}
	projectID, pathOK := h.parsePathProjectID(w, r)
	if !pathOK {
		return 0, 0, false
	}
	if h.LogFilters == nil {
		h.notFound(w, r)
		return 0, 0, false
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return 0, 0, false
	}
	if !canAccess {
		h.notFound(w, r)
		return 0, 0, false
	}
	if !h.parseForm(w, r) {
		return 0, 0, false
	}
	return projectID, uid, true
}

// parseLogFilterID разбирает {filterID} из пути — общий кусок трёх
// хендлеров, работающих с конкретным фильтром (update/delete/default).
func (h *Handler) parseLogFilterID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("filterID"), 10, 64)
	if err != nil || id <= 0 {
		h.notFound(w, r)
		return 0, false
	}
	return id, true
}

// requireLogFilterOperator — вторичный гейт: карта авторизации фиксирует
// минимальный уровень МАРШРУТА (lvlAccess), но право на действие зависит от
// ВИДА фильтра, а не от URL. Создание, изменение и удаление общего фильтра
// (в том числе превращение личного в общий и обратно) требует владельца или
// админа организации — рядовому участнику (в т.ч. присоединённому к команде
// проекта) отвечаем честным 403.
//
// Здесь НАМЕРЕННО не используется canOperateProject/requireProjectOperator
// (team-based, тот же гейт, что у монитора/статус-страниц): его докблок сам
// объясняет, что сегодня это условие СОВПАДАЕТ с CanAccessProject (любой,
// кто прошёл team-attachment, уже «оператор») — секондарный гейт на его
// основе был бы декоративным для маршрута, объявленного lvlAccess. Права на
// общий ресурс проекта здесь берутся строже — requireProjectRole
// (owner/admin организации, projsettings.go), тот же приём, что у admin-
// only настроек проекта.
func (h *Handler) requireLogFilterOperator(w http.ResponseWriter, r *http.Request, projectID, uid int64) bool {
	_, ok := h.requireProjectRole(w, r, projectID, uid)
	return ok
}

// loadOwnedLogFilter читает фильтр по id и проверяет, что он вообще
// принадлежит projectID и виден вызывающему: чужой ЛИЧНЫЙ фильтр отдаёт
// ErrNotFound — существование чужого личного фильтра не подтверждаем (тот
// же приём, что у чужой заявки на выгрузку, exports.go:309). Общий фильтр
// виден всем с доступом к проекту — тут отказа по владению нет, дальнейший
// гейт на оператора делает вызывающий хендлер отдельно (см.
// requireLogFilterOperator), потому что личный, «поднимаемый» до общего,
// тоже обязан пройти этот гейт, хотя ownership-проверка тут его бы пропустила.
func (h *Handler) loadOwnedLogFilter(ctx context.Context, projectID, uid, filterID int64) (logfilter.Filter, error) {
	f, err := h.LogFilters.Get(ctx, filterID)
	if err != nil {
		return logfilter.Filter{}, err
	}
	if f.ProjectID != projectID {
		return logfilter.Filter{}, logfilter.ErrNotFound
	}
	if !f.Shared() && (f.OwnerUserID == nil || *f.OwnerUserID != uid) {
		return logfilter.Filter{}, logfilter.ErrNotFound
	}
	return f, nil
}

// logFiltersCreate — POST /projects/{id}/logs/filters.
func (h *Handler) logFiltersCreate(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.logFiltersGate(w, r)
	if !ok {
		return
	}

	shared := r.PostFormValue("shared") != ""
	if shared && !h.requireLogFilterOperator(w, r, projectID, uid) {
		return
	}

	var ownerUserID *int64
	if !shared {
		ownerUserID = &uid
	}
	preds := logFilterPredicatesFromForm(r)
	name := r.PostFormValue("name")

	if _, err := h.LogFilters.Create(r.Context(), projectID, ownerUserID, uid, name, preds); err != nil {
		h.logFiltersHandleSaveError(w, r, projectID, uid, err)
		return
	}
	h.flashOK(w, "flash.log_filter_saved", 0)
	http.Redirect(w, r, templates.LogsURLFromValues(projectID, logFilterFormParams(r)), http.StatusSeeOther)
}

// logFiltersUpdate — POST /projects/{id}/logs/filters/{filterID}/update.
func (h *Handler) logFiltersUpdate(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.logFiltersGate(w, r)
	if !ok {
		return
	}
	filterID, ok := h.parseLogFilterID(w, r)
	if !ok {
		return
	}
	existing, err := h.loadOwnedLogFilter(r.Context(), projectID, uid, filterID)
	if err != nil {
		if errors.Is(err, logfilter.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	shared := r.PostFormValue("shared") != ""
	// Оператор нужен, если фильтр общий ДО правки, ИЛИ становится общим
	// ПОСЛЕ неё (превращение личного в общий и обратно — тот же гейт, что
	// у самого общего фильтра, брифом задачи это явно оговорено).
	if (existing.Shared() || shared) && !h.requireLogFilterOperator(w, r, projectID, uid) {
		return
	}

	var ownerUserID *int64
	if !shared {
		// Понижение общего до личного делает владельцем того, кто выполняет
		// действие — не прежнего владельца (у общего фильтра его и нет).
		// Личный фильтр, остающийся личным, и так принадлежит uid — иначе
		// loadOwnedLogFilter выше уже отдал бы ErrNotFound.
		ownerUserID = &uid
	}
	preds := logFilterPredicatesFromForm(r)
	name := r.PostFormValue("name")

	if err := h.LogFilters.Update(r.Context(), filterID, name, preds, ownerUserID); err != nil {
		h.logFiltersHandleSaveError(w, r, projectID, uid, err)
		return
	}
	h.flashOK(w, "flash.log_filter_updated", 0)
	http.Redirect(w, r, templates.LogsURLFromValues(projectID, logFilterFormParams(r)), http.StatusSeeOther)
}

// logFiltersDelete — POST /projects/{id}/logs/filters/{filterID}/delete.
func (h *Handler) logFiltersDelete(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.logFiltersGate(w, r)
	if !ok {
		return
	}
	filterID, ok := h.parseLogFilterID(w, r)
	if !ok {
		return
	}
	existing, err := h.loadOwnedLogFilter(r.Context(), projectID, uid, filterID)
	if err != nil {
		if errors.Is(err, logfilter.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if existing.Shared() && !h.requireLogFilterOperator(w, r, projectID, uid) {
		return
	}

	// Умолчания на фильтр у всех пользователей уходят каскадом
	// (log_default_filters.filter_id ON DELETE CASCADE, docblock Store.Delete)
	// — веб-слою ничего досоставлять не нужно.
	if err := h.LogFilters.Delete(r.Context(), filterID); err != nil {
		if errors.Is(err, logfilter.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.log_filter_deleted", 0)
	http.Redirect(w, r, templates.LogsURLFromValues(projectID, logFilterFormParams(r)), http.StatusSeeOther)
}

// logFiltersSetDefault — POST /projects/{id}/logs/filters/{filterID}/default.
//
// Store.SetDefault — единственный метод стора со встроенным гардом
// владения/видимости (его докблок, logfilter/store.go): чужой личный
// фильтр отдаёт ErrNotFound сам, без похода через loadOwnedLogFilter.
// Оператор для общего фильтра тут не нужен: назначение умолчания —
// персональная настройка вызывающего, а не правка самого фильтра.
func (h *Handler) logFiltersSetDefault(w http.ResponseWriter, r *http.Request) {
	projectID, uid, ok := h.logFiltersGate(w, r)
	if !ok {
		return
	}
	filterID, ok := h.parseLogFilterID(w, r)
	if !ok {
		return
	}
	if err := h.LogFilters.SetDefault(r.Context(), projectID, uid, filterID); err != nil {
		if errors.Is(err, logfilter.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.log_filter_default_set", 0)
	http.Redirect(w, r, templates.LogsURLFromValues(projectID, logFilterFormParams(r)), http.StatusSeeOther)
}

// logFiltersHandleSaveError — общий разбор отказа Store.Create/Update:
// ValidationError и сентинелы ErrNameTaken/ErrLimitReached перерисовывают
// страницу логов со статусом 422 и понятным сообщением (тот же приём, что
// renderExportsPage у выгрузок, exports.go:290) — редиректа тут нет, чтобы
// не терять введённые условия. ErrNotFound — фильтр успели удалить в
// параллельном запросе между loadOwnedLogFilter и Update; остальное — 500.
//
// renderLogsPage получает условия ИМЕННО из формы (logFilterFormParams(r)),
// а не из r.URL.Query() (у POST-запроса он пуст, action ведёт на
// /projects/{id}/logs/filters) — иначе введённые условия исчезали бы со
// страницы отказа, а пустой query включал бы фильтр по умолчанию поверх.
func (h *Handler) logFiltersHandleSaveError(w http.ResponseWriter, r *http.Request, projectID, uid int64, err error) {
	params := logFilterFormParams(r)
	var ve *logfilter.ValidationError
	if errors.As(err, &ve) {
		h.renderLogsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, i18n.T(r.Context(), "error.logfilter."+ve.Code), params)
		return
	}
	if errors.Is(err, logfilter.ErrNameTaken) {
		h.renderLogsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, i18n.T(r.Context(), "error.logfilter.name_taken"), params)
		return
	}
	if errors.Is(err, logfilter.ErrLimitReached) {
		h.renderLogsPage(w, r, http.StatusUnprocessableEntity, projectID, uid, i18n.T(r.Context(), "error.logfilter.limit_reached"), params)
		return
	}
	if errors.Is(err, logfilter.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
}

// logFiltersPanel строит вью-модель панели «Мои»/«Общие» для рендера
// страницы логов (renderLogsPage). h.LogFilters == nil (стенд без проводки
// сохранённых фильтров) — пустая панель без похода в БД, тот же принцип
// nil-safety, что у остальных необязательных полей Handler (LogQuery/
// Trace/Profiles).
func (h *Handler) logFiltersPanel(ctx context.Context, projectID, uid int64) templates.LogSavedFiltersPanel {
	if h.LogFilters == nil {
		return templates.LogSavedFiltersPanel{}
	}
	visible, err := h.LogFilters.Visible(ctx, projectID, uid)
	if err != nil {
		slog.Warn("logs: saved filters unavailable", "project_id", projectID, "err", err)
		return templates.LogSavedFiltersPanel{}
	}
	defFilter, hasDefault, err := h.LogFilters.Default(ctx, projectID, uid)
	if err != nil {
		slog.Warn("logs: default filter unavailable", "project_id", projectID, "err", err)
	}
	canOperate, err := h.canManageProject(ctx, projectID, uid)
	if err != nil {
		slog.Warn("logs: operator check unavailable", "project_id", projectID, "err", err)
	}

	panel := templates.LogSavedFiltersPanel{CanShare: canOperate}
	for _, f := range visible {
		canEdit := canOperate
		if !f.Shared() {
			canEdit = f.OwnerUserID != nil && *f.OwnerUserID == uid
		}
		row := templates.LogSavedFilterRow{
			Filter:    f,
			CanEdit:   canEdit,
			IsDefault: hasDefault && defFilter.ID == f.ID,
		}
		if f.Shared() {
			panel.Shared = append(panel.Shared, row)
		} else {
			panel.Personal = append(panel.Personal, row)
		}
	}
	return panel
}
