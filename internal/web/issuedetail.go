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

// issueChartBuckets — целевое число столбиков графика частоты (при окне по
// умолчанию 7д даёт шаг 3ч — прежнее разрешение). Шаг подбирает autoStep по
// выбранному окну; события читаются из сырой events (без 5m-MV), поэтому
// выравнивание не нужно (align=0), лишь пол в 5 минут.
const issueChartBuckets = 56

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

	tr := h.resolveTimeRange(w, r, "7d")
	step := autoStep(tr.Window(), 5*time.Minute, 0, issueChartBuckets)
	selectedID := r.URL.Query().Get("event")

	// Все чтения ClickHouse карточки (события, график, выбранное событие) —
	// одним блоком: отказ хранилища не роняет страницу (единый приём
	// CH-страниц, образец — logsList), шапка, статус и участники (PostgreSQL)
	// остаются, на месте графика и событий — «данные временно недоступны».
	// Первый отказ прекращает опрос хранилища.
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
	// Дозаполняем окно пустыми корзинами, чтобы ось шла по выбранному
	// интервалу целиком.
	points = fillSeries(points, tr.From, tr.To, step,
		func(p event.Point) time.Time { return p.T },
		func(t time.Time) event.Point { return event.Point{T: t} })
	chart := chartSVG(r.Context(), points, chartWidth, chartHeight)

	if selected == nil && len(events) > 0 {
		selected = &events[0]
		selectedID = events[0].ID
		frames = parseStacktraceFrames(events[0].Stacktrace)
	}

	// Ссылку «Смотреть трейс» показываем только если для trace_id реально есть
	// транзакция: при сэмплировании (traces_sample_rate<1) трейс ошибки часто
	// не записан, и страница трейса отдала бы 404 (см. traceWaterfall).
	hasTrace := false
	if selected != nil && selected.TraceID != "" && h.Trace != nil {
		// Проверяем существование трейса В ЭТОМ проекте (project_id уже известен)
		// — префикс первичного ключа прунит запрос до проекта, а не сканирует
		// транзакции всех проектов, как это делал ProjectForTrace на самой
		// частой странице (деталь issue). См. TraceExistsInProject.
		if found, err := h.Trace.TraceExistsInProject(r.Context(), it.ProjectID, selected.TraceID); err == nil {
			hasTrace = found
		}
	}

	// showAllFrames (?frames=all) раскрывает системные кадры стектрейса серверно
	// (строгий CSP запрещает клиентский JS) — переключается ссылкой на странице.
	showAllFrames := r.URL.Query().Get("frames") == "all"

	// Дампы для «Скопировать для ИИ» считаем только при выбранном событии —
	// без него copyToolbar не рендерится (см. issuedetail.templ).
	var copyMD, copyTXT string
	if selected != nil {
		copyMD = renderEventForLLM(it, *selected, dumpMarkdown)
		copyTXT = renderEventForLLM(it, *selected, dumpPlain)
	}

	// exportsEnabled — h.Exports != nil: на инстансе без каталога выгрузок
	// воркер не стартует, форма экспорта событий issue поведёт на 404
	// (ревью веб-части E1, п.3; та же граница, что canExport на issues.go).
	exportsEnabled := h.Exports != nil

	// canManagePII — тот же predicate, что canManage в issues.go (owner/
	// admin организации), и то же правило, что authz.CanManage в
	// exports.go: галка «выгрузить как есть» на раскрытой форме экспорта
	// событий issue видна только ей — от оператора include_pii молча
	// игнорируется на постановке (спека §7/§8, находка аудита P2-UX-3: до
	// этой находки галка не показывалась здесь вовсе, даже CanManage не мог
	// выгрузить события ОДНОЙ issue без маски).
	role, err := h.Org.Role(r.Context(), orgID, uid)
	if err != nil && !errors.Is(err, org.ErrNotMember) {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	canManagePII := role == org.RoleOwner || role == org.RoleAdmin

	_ = templates.IssueDetail(it, members, chart, timeRangeVM(tr), events, selectedID, selected, frames, h.currentEmail(r), hasTrace, showAllFrames, copyMD, copyTXT, exportsEnabled, canManagePII, loadFailed).Render(r.Context(), w)
}

// issueSetStatus — POST /issues/{id}/status: status=unresolved|resolved|ignored
// → 303 обратно на страницу issue.
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
	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.issue_status_saved", 0)
	http.Redirect(w, r, issueDetailPath(it.ID), http.StatusSeeOther)
}

// issueAssign — POST /issues/{id}/assign: assignee=<user id>|"" → 303 обратно
// на страницу issue. assignee должен быть участником организации проекта
// (иначе 422) — та же организация, что отдаёт assign-select на странице.
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
	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
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
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		members, err := h.Org.MembersOf(r.Context(), orgID)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Flash различает назначение и снятие ответственного (K7-9).
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

// exceptionFrame/exceptionValue/exceptionPayload — минимальный локальный
// парсер JSON исключения, хранящегося в event.Stored.Stacktrace:
// {"values":[{"type","value","stacktrace":{"frames":[{"function","module",
// "filename","lineno","in_app"}]}}]}. Не переиспользует internal/ingest
// (у него свой более широкий тип события) — этому пакету нужны только
// фреймы для отображения.
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
