package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const perfIssuesListLimit = 100

func perfIssuesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/perf-issues"
}

func perfIssueDetailPath(id int64) string {
	return "/perf-issues/" + strconv.FormatInt(id, 10)
}

// Дефолт (пустой/неизвестный) — unresolved; "all" даёт пустой status (без фильтра в List).
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
	// h.PerfIssues может быть nil в стендах без детекции — 404, а не паника при разыменовании.
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

	// Кнопки статуса рендерятся безусловно: POST-обработчик проверяет ту же границу
	// доступа, отдельная роль-проверка тут не нужна.
	data := templates.PerfIssueDetailData{
		Issue:    iss,
		Evidence: parsePerfEvidence(iss.Evidence),
	}
	// Любая ошибка/истёкший трейс — не критично, страница рисуется и без обогащения.
	if h.Trace != nil && iss.SampleTraceID != "" {
		if ids := perfEvidenceSpanIDs(iss.Evidence); len(ids) > 0 {
			if spans, err := h.Trace.OffendingSpans(r.Context(), iss.ProjectID, iss.SampleTraceID, ids); err == nil {
				enrichPerfDetail(&data, spans)
			}
		}
	}
	_ = templates.PerfIssueDetail(data, h.currentEmail(r)).Render(r.Context(), w)
}

func perfEvidenceSpanIDs(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var m struct {
		SpanIDs []string `json:"span_ids"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m.SpanIDs
}

// Берём первый спан с непустым описанием (спаны отсортированы по длительности убыв.),
// иначе первый — например, http-флуд без текста запроса, но с привязкой к коду.
func enrichPerfDetail(d *templates.PerfIssueDetailData, spans []trace.SpanDetail) {
	if len(spans) == 0 {
		return
	}
	rep := spans[0]
	for _, s := range spans {
		if s.Description != "" {
			rep = s
			break
		}
	}
	d.Query = rep.Description
	d.QueryOp = rep.Op
	d.SpanDurationUS = int64(rep.DurationUS)
	d.DBSystem = rep.Data["db.system"]
	d.Code = codeLocFromData(rep.Data)
}

func codeLocFromData(data map[string]string) *templates.PerfCodeLoc {
	if len(data) == 0 {
		return nil
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := data[k]; v != "" {
				return v
			}
		}
		return ""
	}
	file := pick("code.filepath", "code.file", "filename")
	fn := pick("code.function", "code.namespace")
	if file == "" && fn == "" {
		return nil
	}
	return &templates.PerfCodeLoc{
		File:     file,
		Line:     pick("code.lineno", "code.line"),
		Function: fn,
	}
}

func (h *Handler) perfIssueSetStatus(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	// Резолвим проект ДО проверки доступа: несуществующая проблема должна давать тот же
	// 404, что и чужая — иначе разные тела ответов выдавали бы существование id.
	projectID, found, err := h.PerfIssues.ProjectOf(r.Context(), id)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !found {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !canAccess {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	status := r.FormValue("status")
	if err := h.PerfIssues.SetStatus(r.Context(), projectID, id, status); err != nil {
		if errors.Is(err, trace.ErrInvalidStatus) {
			h.renderError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "error.perfissue.invalid_status"))
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

// Невалидный/пустой JSON — пустое представление, не ошибка: страница рисуется в любом
// случае, флаги Has* остаются false.
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
