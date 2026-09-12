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
	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

const (
	sparklineWindow  = 24 * time.Hour
	sparklineBuckets = 24
)

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

	// Ссылка на «Project settings» требует owner/admin — member её не должен видеть вовсе.
	// org.ErrNotMember не роняет страницу: значит доступ был только через команду.
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
	// «Параметр не задан» (дефолт unresolved) отличается от «явно выбрано Все» (?status= пусто) —
	// так дефолт остаётся переопределяемым из UI.
	status := q.Get("status")
	if !q.Has("status") {
		status = issue.StatusUnresolved
	}
	// «За всё время» по умолчанию: большинство групп старше суток, окно 24ч показало бы
	// пустой список на здоровом проекте.
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

	// Единственное чтение ClickHouse на странице; отказ не роняет её — колонка спарклайнов
	// пустая, «графики временно недоступны».
	sparklines, err := h.sparklinesFor(r.Context(), projectID, items)
	sparklinesFailed := err != nil
	if sparklinesFailed {
		slog.Warn("issues: sparklines failed", "project_id", projectID, "err", err)
		sparklines = nil
	}

	rows := make([]templates.IssueRow, len(items))
	for i, it := range items {
		rows[i] = templates.IssueRow{
			Issue:     it,
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
		// «Активен», если сужает список относительно умолчаний (unresolved + всё время) — явные
		// ?status=Все/unresolved не в счёт, иначе пустой список выглядел бы как «ничего не подошло».
		Active: filter.Level != "" || filter.Query != "" || filter.Environment != "" ||
			(q.Has("status") && status != "" && status != issue.StatusUnresolved) ||
			rng.Key != RangeAll,
	}
	banner := h.quotaBanner(r.Context(), orgID, canManage)
	// canOperateProject, не canAccess: сегодня предикаты совпадают и это ничего не меняет, но
	// при их расхождении CanOperate ниже обязана следовать сама, без правки этого места.
	canOperate, err := h.canOperateProject(r.Context(), projectID, uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	gs := h.gettingStarted(r.Context(), uid, projectID, orgID, canManage, canOperate)
	// h.Exports == nil на инстансе без каталога выгрузок — воркер не стартовал, кнопка ведёт на 404.
	canExport := canOperate && h.Exports != nil
	// Галка «выгрузить как есть» видна только owner/admin — оператору include_pii молча
	// игнорируется на бэкенде, здесь просто не рендерим то, чем нельзя воспользоваться.
	_ = templates.IssuesList(projectID, rows, tplFilter, page, total, h.currentEmail(r), environments, banner, gs, canExport, canManage, sparklinesFailed).Render(r.Context(), w)
}

// Шаг 1 всегда закрыт — страница issues уже требует существующий проект. Недостающие сервисы
// (h.Alerts/h.Uptime nil) или их ошибки не роняют страницу — шаг просто «ещё не закрыт».
func (h *Handler) gettingStarted(ctx context.Context, uid, projectID, orgID int64, canManage, canOperate bool) templates.GettingStartedVM {
	gs := templates.GettingStartedVM{ProjectID: projectID, OrgID: orgID, CanManage: canManage, CanOperate: canOperate}

	// KeyRejects собирается даже когда чек-лист скрыт (Hidden) — единственный способ увидеть
	// отказы по ключу в пустом состоянии issues.
	hidden, err := h.Auth.HideGettingStarted(ctx, uid)
	if err != nil {
		slog.Warn("gettingStarted: hide flag lookup failed", "user_id", uid, "err", err)
	}
	gs.Hidden = hidden

	if !hidden {
		if exists, err := h.Issues.Exists(ctx, projectID); err != nil {
			slog.Warn("gettingStarted: issues exists check failed", "project_id", projectID, "err", err)
		} else {
			gs.Step2Done = exists
		}

		// Channels() напрямую, мимо channelsForView: только len(channels) идёт наружу,
		// Target/Secret никогда не покидают функцию — маскировать нечего.
		if h.Alerts == nil {
			slog.Warn("gettingStarted: Alerts service not configured", "project_id", projectID)
		} else if channels, err := h.Alerts.Channels(ctx, projectID); err != nil {
			slog.Warn("gettingStarted: alert channels failed", "project_id", projectID, "err", err)
		} else {
			gs.Step3Done = len(channels) > 0
		}

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
	}

	// h.Signals — необязательное поле, как Deploy/Trace (не как Alerts/Uptime выше): nil просто
	// скрывает секцию, без Warn в логе.
	if h.Signals != nil {
		if signals, err := h.Signals.ForProject(ctx, projectID); err != nil {
			slog.Warn("gettingStarted: ingest signals failed", "project_id", projectID, "err", err)
		} else {
			cutoff := time.Now().Add(-time.Hour)
			for _, sig := range signals {
				if !isKeyRejectKind(sig.Kind) || sig.LastSeenAt.Before(cutoff) {
					continue
				}
				gs.KeyRejects = append(gs.KeyRejects, templates.KeyRejectView{
					Kind:       string(sig.Kind),
					Hits:       sig.Hits,
					LastSeenAt: sig.LastSeenAt,
				})
			}
		}
	}

	gs.Done = 1
	for _, done := range []bool{gs.Step2Done, gs.Step3Done, gs.Step4aDone, gs.Step4bDone} {
		if done {
			gs.Done++
		}
	}
	return gs
}

// Отказы по ключу, не устаревшие пути приёма — те показываются отдельно, в настройках проекта.
func isKeyRejectKind(k ingestsignal.Kind) bool {
	switch k {
	case ingestsignal.KindKeyInvalid, ingestsignal.KindKeyProjectMismatch, ingestsignal.KindKeyScope:
		return true
	default:
		return false
	}
}

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

// Без потолка `?page=<огромное>` даёт переполнение int в (page-1)*perPage → отрицательный
// SQL OFFSET → 500. Значение заведомо выше любого реального набора данных.
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

var bulkActionStatus = map[string]string{
	"resolve":   issue.StatusResolved,
	"ignore":    issue.StatusIgnored,
	"unresolve": issue.StatusUnresolved,
}

var bulkActionFlashKey = map[string]string{
	"resolve":   "flash.issues_resolved",
	"ignore":    "flash.issues_ignored",
	"unresolve": "flash.issues_reopened",
}

// Экспортируется для internal/guards: ключ уходит в flashOK динамически (карта не литерал),
// сканер по местам вызова его не видит — множество значений должно читаться отсюда.
var BulkActionFlashKeys = func() []string {
	out := make([]string, 0, len(bulkActionFlashKey))
	for _, key := range bulkActionFlashKey {
		out = append(out, key)
	}
	return out
}()

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

	if !h.parseForm(w, r) {
		return
	}
	status, ok := bulkActionStatus[r.FormValue("action")]
	if !ok {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	ids := parseIDs(r.Form["ids"])
	if len(ids) == 0 {
		h.flashWarn(w, "flash.nothing_selected", 0)
		http.Redirect(w, r, BulkRedirectTarget(r, h.BaseURL, projectID), http.StatusSeeOther)
		return
	}
	n, err := h.Issues.SetStatusBulk(r.Context(), projectID, ids, status)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Число входит во flash-сообщение: без него нельзя понять, сработало ли и на скольких.
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

// Отвергает "//" и "/\" — браузеры нормализуют обратный слэш в прямой при навигации, так что
// "/\evil.com" стал бы "//evil.com" (protocol-relative обход) — открытый редирект.
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
