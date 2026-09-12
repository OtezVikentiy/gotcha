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

// Список закрытый и общий для обеих операций (предикаты и адрес возврата) — произвольное
// поле формы (вроде "back") в редирект никогда не попадает, что закрывает открытый редирект.
var logFilterParams = []string{
	"severity", "service", "environment", "q", "attr", "trace_id",
	"q_not", "severity_not", "service_not", "environment_not", "attr_not",
}

// Тем же списком строятся и предикаты для сохранения, и адрес возврата — расхождение между
// «что сохранили» и «куда вернулись» невозможно по построению.
func logFilterFormParams(r *http.Request) url.Values {
	q := url.Values{}
	for _, name := range logFilterParams {
		if vs, ok := r.PostForm[name]; ok {
			q[name] = vs
		}
	}
	return q
}

// TimeRange{} и retentionDays=0 не участвуют в результате: filterToPredicates не читает
// From/To, сохранённый фильтр не несёт временное окно.
func logFilterPredicatesFromForm(r *http.Request) []log.Predicate {
	f, _ := parseLogFilter(logFilterFormParams(r), TimeRange{}, 0)
	return filterToPredicates(f)
}

// ok=false — проверка уже отправила ответ, вызывающему остаётся просто вернуться.
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

func (h *Handler) parseLogFilterID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("filterID"), 10, 64)
	if err != nil || id <= 0 {
		h.notFound(w, r)
		return 0, false
	}
	return id, true
}

// Не canOperateProject: та сегодня совпадает с CanAccessProject (уже пройдено гейтом маршрута) —
// секондарный гейт на её основе был бы декоративным. Здесь требуется requireProjectRole (owner/admin).
func (h *Handler) requireLogFilterOperator(w http.ResponseWriter, r *http.Request, projectID, uid int64) bool {
	_, ok := h.requireProjectRole(w, r, projectID, uid)
	return ok
}

// Чужой личный фильтр — ErrNotFound, не 403: не подтверждаем его существование. Общий виден
// всем с доступом к проекту — гейт на оператора делает вызывающий хендлер отдельно.
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
	// Оператор нужен, если фильтр общий ДО правки, ИЛИ становится общим ПОСЛЕ — тот же гейт
	// в обе стороны превращения личный/общий.
	if (existing.Shared() || shared) && !h.requireLogFilterOperator(w, r, projectID, uid) {
		return
	}

	var ownerUserID *int64
	if !shared {
		// Понижение общего до личного делает владельцем того, кто выполняет действие, не прежнего
		// (у общего фильтра владельца и нет).
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

	// Умолчания на фильтр каскадом (ON DELETE CASCADE) — веб-слою ничего досоставлять не нужно.
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

// Store.SetDefault сам гардит владение/видимость — чужой личный фильтр отдаёт ErrNotFound без
// loadOwnedLogFilter. Оператор для общего фильтра не нужен: это личная настройка вызывающего.
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

// params — из формы (logFilterFormParams), не r.URL.Query() (пуст у POST): иначе введённые
// условия исчезли бы со страницы отказа, а пустой query включил бы фильтр по умолчанию.
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

// nil-safe: без проводки сохранённых фильтров — пустая панель без похода в БД.
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
