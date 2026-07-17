package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// perfIssuesListLimit — потолок выборки списка perf-проблем на странице (тот же
// порядок величины, что и perfIssuesLimit на странице эндпойнта).
const perfIssuesListLimit = 100

func perfIssuesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/perf-issues"
}

func perfIssueDetailPath(id int64) string {
	return "/perf-issues/" + strconv.FormatInt(id, 10)
}

// perfIssueStatusFilter переводит query-параметр status в аргумент
// IssueService.List и нормализованное имя фильтра для формы. Дефолт (пустой или
// неизвестный) — unresolved: страница по умолчанию показывает то, что требует
// внимания. "all" → пустой status (без фильтра в List).
func perfIssueStatusFilter(v string) (status, name string) {
	switch v {
	case "unresolved", "resolved", "ignored":
		return v, v
	case "all":
		return "", "all"
	default:
		return "unresolved", "unresolved"
	}
}

// perfIssuesList — GET /projects/{id}/perf-issues: таблица perf-проблем проекта
// (доступ — CanAccessProject, иначе 404, тот же принцип, что и у performanceList).
func (h *Handler) perfIssuesList(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// h.PerfIssues может быть nil в стендах без детекции — тогда 404, как и при
	// отсутствии доступа (тот же приём, что и guard на h.Trace в performanceList),
	// а не паника при разыменовании.
	if h.PerfIssues == nil {
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

	status, filterName := perfIssueStatusFilter(r.URL.Query().Get("status"))
	items, err := h.PerfIssues.List(r.Context(), projectID, status, perfIssuesListLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	_ = templates.PerfIssuesList(projectID, items, filterName, h.currentEmail(r)).Render(r.Context(), w)
}

// loadAccessiblePerfIssue — общая часть GET-обработчика страницы проблемы:
// резолвит владеющий проблемой проект (ProjectOf), проверяет доступ к нему
// (CanAccessProject) и читает строку уже скоуплено (Get). Отсутствующая
// проблема и проблема чужого проекта одинаково дают 404 — не палим
// существование чужих числовых id (тот же принцип, что и loadAccessibleIssue).
func (h *Handler) loadAccessiblePerfIssue(w http.ResponseWriter, r *http.Request, uid int64) (trace.PerfIssue, bool) {
	if h.PerfIssues == nil {
		h.notFound(w, r)
		return trace.PerfIssue{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return trace.PerfIssue{}, false
	}
	projectID, found, err := h.PerfIssues.ProjectOf(r.Context(), id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return trace.PerfIssue{}, false
	}
	if !found {
		h.notFound(w, r)
		return trace.PerfIssue{}, false
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return trace.PerfIssue{}, false
	}
	if !canAccess {
		h.notFound(w, r)
		return trace.PerfIssue{}, false
	}
	iss, err := h.PerfIssues.Get(r.Context(), projectID, id)
	if err != nil {
		if errors.Is(err, trace.ErrNotFound) {
			h.notFound(w, r)
			return trace.PerfIssue{}, false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return trace.PerfIssue{}, false
	}
	return iss, true
}

// perfIssueDetail — GET /perf-issues/{id}: заголовок, kind, culprit, счётчик,
// first/last seen, статус, распарсенный evidence и ссылка на пример трейса.
// Кнопки resolve/ignore/unresolve видны только owner/admin (canManage), как и
// POST их принимает только от них (см. perfIssueSetStatus).
func (h *Handler) perfIssueDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	iss, ok := h.loadAccessiblePerfIssue(w, r, uid)
	if !ok {
		return
	}

	// canManage — видны ли кнопки смены статуса: они ведут на POST, который сам
	// требует owner/admin, поэтому member их видеть не должен вовсе. ErrNotMember
	// не должен ронять страницу — доступ к проекту у члена мог быть только через
	// команду (тот же приём, что и в issuesList).
	orgID, err := h.Org.ProjectOrg(r.Context(), iss.ProjectID)
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

	data := templates.PerfIssueDetailData{
		Issue:     iss,
		Evidence:  parsePerfEvidence(iss.Evidence),
		CanManage: canManage,
	}
	_ = templates.PerfIssueDetail(data, h.currentEmail(r)).Render(r.Context(), w)
}

// perfIssueSetStatus — POST /perf-issues/{id}/status: status из формы
// (unresolved|resolved|ignored) → SetStatus → 303 назад на страницу проблемы.
// Смена статуса — только owner/admin (requireProjectRole по проекту проблемы);
// чужой проект и несуществующая проблема → 404, неизвестный статус → 422.
func (h *Handler) perfIssueSetStatus(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if h.PerfIssues == nil {
		h.notFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	// Проект резолвим ДО проверки роли: requireProjectRole проверяет роль в
	// организации проекта проблемы. Несуществующая проблема (found=false) даёт
	// тот же стилизованный 404, что и member на чужой POST (requireProjectRole),
	// — иначе разные тела ответа выдавали бы существование id (enumeration).
	projectID, found, err := h.PerfIssues.ProjectOf(r.Context(), id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !found {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	status := r.FormValue("status")
	if err := h.PerfIssues.SetStatus(r.Context(), projectID, id, status); err != nil {
		if errors.Is(err, trace.ErrInvalidStatus) {
			http.Error(w, "bad status", http.StatusUnprocessableEntity)
			return
		}
		if errors.Is(err, trace.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, perfIssueDetailPath(id), http.StatusSeeOther)
}

// parsePerfEvidence разбирает JSONB evidence в типизированное представление для
// шаблона. Ключи — ровно те, что пишет детектор (см. internal/trace/detect.go):
// count, total_us, max_us (slow_db_query), sequential_pct/max_concurrency/urls
// (http_flood), parent_op (n_plus_one). Невалидный/пустой JSON — пустое
// представление, а не ошибка: страница проблемы должна отрисоваться в любом
// случае (флаги Has* остаются false, соответствующие строки просто не
// показываются).
func parsePerfEvidence(raw []byte) templates.PerfEvidence {
	var ev templates.PerfEvidence
	if len(raw) == 0 {
		return ev
	}
	// total_us/max_us пишутся как int64, но через JSON приезжают числами —
	// читаем в float64 и приводим, как это делает CH-агрегация в других местах.
	var m struct {
		Count          *int64   `json:"count"`
		TotalUS        *float64 `json:"total_us"`
		MaxUS          *float64 `json:"max_us"`
		ParentOp       string   `json:"parent_op"`
		SequentialPct  *float64 `json:"sequential_pct"`
		MaxConcurrency *float64 `json:"max_concurrency"`
		URLs           []string `json:"urls"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ev
	}
	if m.Count != nil {
		ev.Count = *m.Count
	}
	if m.TotalUS != nil {
		ev.TotalUS = int64(*m.TotalUS)
		ev.HasTotal = true
	}
	if m.MaxUS != nil {
		ev.MaxUS = int64(*m.MaxUS)
		ev.HasMax = true
	}
	ev.ParentOp = m.ParentOp
	if m.SequentialPct != nil {
		ev.SequentialPct = int(*m.SequentialPct)
		ev.HasSequential = true
	}
	if m.MaxConcurrency != nil {
		ev.MaxConcurrency = int(*m.MaxConcurrency)
	}
	ev.URLs = m.URLs
	return ev
}
