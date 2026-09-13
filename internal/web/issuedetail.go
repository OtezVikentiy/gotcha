package web

import (
	"encoding/json"
	"errors"
	"log/slog"
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

// События читаются из сырой events (без 5m-MV) — выравнивание не нужно, только пол в 5 минут.
const issueChartBuckets = 56

const issueEventsLimit = 20

func issueDetailPath(issueID int64) string {
	return "/issues/" + strconv.FormatInt(issueID, 10)
}

// Issue не существует и issue чужого проекта — оба 404: не палим существование чужих id.
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
		h.renderError(w, r, http.StatusInternalServerError, "")
		return issue.Issue{}, false
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, it.ProjectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return issue.Issue{}, false
	}
	if !canAccess {
		h.notFound(w, r)
		return issue.Issue{}, false
	}
	return it, true
}

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
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	members, err := h.Org.MembersOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}

	tr := h.resolveTimeRange(w, r, "7d")
	step := autoStep(tr.Window(), 5*time.Minute, 0, issueChartBuckets)
	selectedID := r.URL.Query().Get("event")

	// Чтения ClickHouse — одним блоком: отказ не роняет страницу, PostgreSQL-часть остаётся,
	// на месте графика/событий — «данные временно недоступны».
	var (
		events   []event.Stored
		points   []event.Point
		selected *event.Stored
		frames   []templates.Frame
	)
	loadErr := func() error {
		var err error
		if events, err = h.Events.EventsForIssue(r.Context(), it.ProjectID, it.ID, issueEventsLimit); err != nil {
			return err
		}
		if points, err = h.Events.Series(r.Context(), it.ProjectID, it.ID, tr.From, tr.To, step); err != nil {
			return err
		}
		if selectedID != "" {
			if _, err := uuid.Parse(selectedID); err == nil {
				ev, found, err := h.Events.EventByID(r.Context(), it.ProjectID, selectedID)
				if err != nil {
					return err
				}
				if found {
					selected = &ev
					frames = parseStacktraceFrames(ev.Stacktrace)
				}
			}
		}
		return nil
	}()
	loadFailed := loadErr != nil
	if loadFailed {
		slog.Warn("issues: detail events failed", "project_id", it.ProjectID, "issue_id", it.ID, "err", loadErr)
		events, points, selected, frames = nil, nil, nil, nil
	}
	points = fillSeries(points, tr.From, tr.To, step,
		func(p event.Point) time.Time { return p.T },
		func(t time.Time) event.Point { return event.Point{T: t} })
	chart := chartSVG(r.Context(), points, chartWidth, chartHeight)

	if selected == nil && len(events) > 0 {
		selected = &events[0]
		selectedID = events[0].ID
		frames = parseStacktraceFrames(events[0].Stacktrace)
	}

	// Ссылку показываем только если транзакция реально записана: при sampling<1 трейс ошибки
	// часто не сохранён, и страница трейса отдала бы 404.
	hasTrace := false
	if selected != nil && selected.TraceID != "" && h.Trace != nil {
		// Префикс первичного ключа прунит запрос до проекта, не сканирует транзакции всех проектов.
		if found, err := h.Trace.TraceExistsInProject(r.Context(), it.ProjectID, selected.TraceID); err == nil {
			hasTrace = found
		}
	}

	// Раскрывает системные кадры стектрейса серверно: строгий CSP запрещает клиентский JS.
	showAllFrames := r.URL.Query().Get("frames") == "all"

	var copyMD, copyTXT string
	if selected != nil {
		copyMD = renderEventForLLM(it, *selected, dumpMarkdown)
		copyTXT = renderEventForLLM(it, *selected, dumpPlain)
	}

	// h.Exports == nil на инстансе без каталога выгрузок — воркер не стартовал, форма ведёт на 404.
	exportsEnabled := h.Exports != nil

	// Галка «выгрузить как есть» видна только owner/admin — у оператора include_pii
	// молча игнорируется на постановке.
	role, err := h.Org.Role(r.Context(), orgID, uid)
	if err != nil && !errors.Is(err, org.ErrNotMember) {
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	canManagePII := role == org.RoleOwner || role == org.RoleAdmin

	_ = templates.IssueDetail(it, members, chart, timeRangeVM(tr), events, selectedID, selected, frames, h.currentEmail(r), hasTrace, showAllFrames, copyMD, copyTXT, exportsEnabled, canManagePII, loadFailed, h.RetentionDays).Render(r.Context(), w)
}

func (h *Handler) issueSetStatus(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}
	status := r.FormValue("status")
	if err := h.Issues.SetStatus(r.Context(), it.ID, status); err != nil {
		if errors.Is(err, issue.ErrInvalidStatus) {
			h.renderError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "error.issue.invalid_status"))
			return
		}
		if errors.Is(err, issue.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.flashOK(w, "flash.issue_status_saved", 0)
	http.Redirect(w, r, issueDetailPath(it.ID), http.StatusSeeOther)
}

func (h *Handler) issueAssign(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if !h.parseForm(w, r) {
		return
	}

	raw := r.FormValue("assignee")
	var assigneeID *int64
	if raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			h.renderError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "error.bad_request"))
			return
		}
		orgID, err := h.Org.ProjectOrg(r.Context(), it.ProjectID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		members, err := h.Org.MembersOf(r.Context(), orgID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "")
			return
		}
		if !isOrgMember(members, id) {
			h.renderError(w, r, http.StatusUnprocessableEntity, i18n.T(r.Context(), "error.issue.assignee_not_member"))
			return
		}
		assigneeID = &id
	}

	if err := h.Issues.Assign(r.Context(), it.ID, assigneeID); err != nil {
		if errors.Is(err, issue.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, "")
		return
	}
	if assigneeID != nil {
		h.flashOK(w, "flash.issue_assigned", 0)
	} else {
		h.flashOK(w, "flash.issue_unassigned", 0)
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

// Минимальный локальный парсер JSON исключения из event.Stored.Stacktrace — не переиспользует
// internal/ingest (свой более широкий тип): этому пакету нужны только фреймы для отображения.
type exceptionFrame struct {
	Function    string          `json:"function"`
	Module      string          `json:"module"`
	Filename    string          `json:"filename"`
	Lineno      int             `json:"lineno"`
	InApp       bool            `json:"in_app"`
	AbsPath     string          `json:"abs_path"`
	ContextLine string          `json:"context_line"`
	PreContext  []string        `json:"pre_context"`
	PostContext []string        `json:"post_context"`
	Vars        json.RawMessage `json:"vars"`
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

// Фреймы — в обратном порядке (новые сверху); `values` — от первопричины к внешнему исключению,
// берём последний, как и internal/fingerprint/fingerprint.go. Пустой/невалидный JSON — nil, не ошибка.
func parseStacktraceFrames(raw string) []templates.Frame {
	if raw == "" {
		return nil
	}
	var payload exceptionPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || len(payload.Values) == 0 {
		return nil
	}
	frames := payload.Values[len(payload.Values)-1].Stacktrace.Frames
	out := make([]templates.Frame, len(frames))
	for i, f := range frames {
		out[len(frames)-1-i] = templates.Frame{
			Function:    f.Function,
			Module:      f.Module,
			Filename:    f.Filename,
			Lineno:      f.Lineno,
			InApp:       f.InApp,
			AbsPath:     f.AbsPath,
			ContextLine: f.ContextLine,
			PreContext:  f.PreContext,
			PostContext: f.PostContext,
			Vars:        string(f.Vars),
		}
	}
	return out
}
