package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// issueChartWindow/Step — окно и шаг графика частоты на странице issue:
// 7 дней с шагом 3 часа даёт 56 точек — разумное разрешение для bar-chart
// шириной chartWidth (см. svg.go).
const (
	issueChartWindow = 7 * 24 * time.Hour
	issueChartStep   = 3 * time.Hour
)

// issueEventsLimit — сколько последних событий issue показывается списком.
const issueEventsLimit = 20

func issueDetailPath(issueID int64) string {
	return "/issues/" + strconv.FormatInt(issueID, 10)
}

// loadAccessibleIssue — общая часть GET/POST issue-обработчиков: находит
// issue по id и проверяет, что текущий юзер видит его проект. Оба случая
// (issue не существует, issue существует но проект чужой) отдают 404 —
// не палим существование чужих числовых id, тот же принцип, что и в
// issuesList/projectSetup.
func (h *Handler) loadAccessibleIssue(w http.ResponseWriter, r *http.Request, uid int64) (issue.Issue, bool) {
	issueID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return issue.Issue{}, false
	}
	it, err := h.Issues.Get(r.Context(), issueID)
	if err != nil {
		if errors.Is(err, issue.ErrNotFound) {
			h.notFound(w, r)
			return issue.Issue{}, false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return issue.Issue{}, false
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, it.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return issue.Issue{}, false
	}
	if !canAccess {
		h.notFound(w, r)
		return issue.Issue{}, false
	}
	return it, true
}

// issueDetail — GET /issues/{id}: шапка, статус/assign-формы, график за
// 7 дней, последние 20 событий, детали ?event=<id> (стектрейс, tags, user,
// sdk, contexts).
func (h *Handler) issueDetail(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	it, ok := h.loadAccessibleIssue(w, r, uid)
	if !ok {
		return
	}

	orgID, err := h.Org.ProjectOrg(r.Context(), it.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	members, err := h.Org.MembersOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	events, err := h.Events.EventsForIssue(r.Context(), it.ProjectID, it.ID, issueEventsLimit)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}

	now := time.Now().UTC()
	points, err := h.Events.Series(r.Context(), it.ProjectID, it.ID, now.Add(-issueChartWindow), now, issueChartStep)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	chart := chartSVG(r.Context(), points, chartWidth, chartHeight)

	selectedID := r.URL.Query().Get("event")
	var selected *event.Stored
	var frames []templates.Frame
	if selectedID != "" {
		// Validate event ID is a valid UUID before calling EventByID.
		// On parse failure, treat as no selection (degrade gracefully).
		if _, err := uuid.Parse(selectedID); err == nil {
			ev, found, err := h.Events.EventByID(r.Context(), it.ProjectID, selectedID)
			if err != nil {
				h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
				return
			}
			if found {
				selected = &ev
				frames = parseStacktraceFrames(ev.Stacktrace)
			}
		}
	}
	// По умолчанию (нет ?event= или выбранное событие устарело/удалено)
	// показываем последнее событие: стектрейс и структурированные данные
	// видны сразу при открытии issue, без явного клика — как в Sentry/
	// GlitchTip. events отсортированы timestamp DESC, значит [0] — свежайшее.
	if selected == nil && len(events) > 0 {
		selected = &events[0]
		selectedID = events[0].ID
		frames = parseStacktraceFrames(events[0].Stacktrace)
	}

	_ = templates.IssueDetail(it, members, chart, events, selectedID, selected, frames, h.currentEmail(r)).Render(r.Context(), w)
}

// issueSetStatus — POST /issues/{id}/status: status=unresolved|resolved|ignored
// → 303 обратно на страницу issue.
func (h *Handler) issueSetStatus(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	it, ok := h.loadAccessibleIssue(w, r, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	status := r.FormValue("status")
	if err := h.Issues.SetStatus(r.Context(), it.ID, status); err != nil {
		if errors.Is(err, issue.ErrInvalidStatus) {
			http.Error(w, "bad status", http.StatusUnprocessableEntity)
			return
		}
		if errors.Is(err, issue.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, issueDetailPath(it.ID), http.StatusSeeOther)
}

// issueAssign — POST /issues/{id}/assign: assignee=<user id>|"" → 303 обратно
// на страницу issue. assignee должен быть участником организации проекта
// (иначе 422) — та же организация, что отдаёт assign-select на странице.
func (h *Handler) issueAssign(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	it, ok := h.loadAccessibleIssue(w, r, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	raw := r.FormValue("assignee")
	var assigneeID *int64
	if raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.Error(w, "bad assignee", http.StatusUnprocessableEntity)
			return
		}
		orgID, err := h.Org.ProjectOrg(r.Context(), it.ProjectID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		members, err := h.Org.MembersOf(r.Context(), orgID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		if !isOrgMember(members, id) {
			http.Error(w, "assignee is not a member of the project's organization", http.StatusUnprocessableEntity)
			return
		}
		assigneeID = &id
	}

	if err := h.Issues.Assign(r.Context(), it.ID, assigneeID); err != nil {
		if errors.Is(err, issue.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, issueDetailPath(it.ID), http.StatusSeeOther)
}

func isOrgMember(members []org.Member, userID int64) bool {
	for _, m := range members {
		if m.UserID == userID {
			return true
		}
	}
	return false
}

// exceptionFrame/exceptionValue/exceptionPayload — минимальный локальный
// парсер JSON исключения, хранящегося в event.Stored.Stacktrace:
// {"values":[{"type","value","stacktrace":{"frames":[{"function","module",
// "filename","lineno","in_app"}]}}]}. Не переиспользует internal/ingest
// (у него свой более широкий тип события) — этому пакету нужны только
// фреймы для отображения.
type exceptionFrame struct {
	Function string `json:"function"`
	Module   string `json:"module"`
	Filename string `json:"filename"`
	Lineno   int    `json:"lineno"`
	InApp    bool   `json:"in_app"`
}

type exceptionValue struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Stacktrace struct {
		Frames []exceptionFrame `json:"frames"`
	} `json:"stacktrace"`
}

type exceptionPayload struct {
	Values []exceptionValue `json:"values"`
}

// parseStacktraceFrames разбирает exception-JSON первого value и возвращает
// фреймы в обратном порядке (новые/самые глубокие — сверху), как того
// требует UI. Невалидный/пустой JSON и отсутствие фреймов — пустой результат,
// а не ошибка: страница issue должна отрисоваться даже без стектрейса
// (например, событие без исключения, просто message).
func parseStacktraceFrames(raw string) []templates.Frame {
	if raw == "" {
		return nil
	}
	var payload exceptionPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || len(payload.Values) == 0 {
		return nil
	}
	frames := payload.Values[0].Stacktrace.Frames
	out := make([]templates.Frame, len(frames))
	for i, f := range frames {
		out[len(frames)-1-i] = templates.Frame{
			Function: f.Function,
			Module:   f.Module,
			Filename: f.Filename,
			Lineno:   f.Lineno,
			InApp:    f.InApp,
		}
	}
	return out
}
