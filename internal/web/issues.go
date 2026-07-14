package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
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
		http.NotFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !canAccess {
		http.NotFound(w, r)
		return
	}

	// canManage — виден ли «Project settings» (dead link fix, задача 5/2):
	// ссылка ведёт на страницу, которая сама требует owner/admin, поэтому
	// member её видеть не должен вовсе. org.ErrNotMember не должен ронять
	// страницу — это лишь означает, что показывать ссылку не нужно (доступ к
	// проекту у юзера мог быть только через команду).
	orgID, err := h.Org.ProjectOrg(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	role, err := h.Org.Role(r.Context(), orgID, uid)
	if err != nil && !errors.Is(err, org.ErrNotMember) {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	canManage := role == org.RoleOwner || role == org.RoleAdmin

	q := r.URL.Query()
	filter := issue.Filter{
		Status: q.Get("status"),
		Level:  q.Get("level"),
		Query:  q.Get("q"),
		Sort:   q.Get("sort"),
		Page:   parsePage(q.Get("page")),
	}

	items, total, err := h.Issues.List(r.Context(), projectID, filter)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	sparklines, err := h.sparklinesFor(r.Context(), projectID, items)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	rows := make([]templates.IssueRow, len(items))
	for i, it := range items {
		rows[i] = templates.IssueRow{
			Issue:     it,
			Sparkline: sparklineSVG(sparklines[it.ID], sparklineWidth, sparklineHeight),
		}
	}

	page := filter.Page
	if page < 1 {
		page = 1
	}
	tplFilter := templates.IssuesFilter{
		Status: filter.Status,
		Level:  filter.Level,
		Query:  filter.Query,
		Sort:   filter.Sort,
	}
	_ = templates.IssuesList(projectID, rows, tplFilter, page, total, canManage, h.currentEmail(r)).Render(r.Context(), w)
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

func parsePage(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// bulkActionStatus — whitelist допустимых POST-действий bulk-панели.
var bulkActionStatus = map[string]string{
	"resolve":   "resolved",
	"ignore":    "ignored",
	"unresolve": "unresolved",
}

// issuesBulk — POST /projects/{id}/issues/bulk: action=resolve|ignore|unresolve
// + ids[] → SetStatusBulk → 303. Редирект идёт на Referer (сохраняет текущие
// фильтры/страницу), если Referer same-origin, иначе на список issues без
// query — тот же принцип sameOrigin, что и у остальных POST в этом пакете.
func (h *Handler) issuesBulk(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	canAccess, err := h.Org.CanAccessProject(r.Context(), uid, projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !canAccess {
		http.NotFound(w, r)
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
	if len(ids) > 0 {
		if _, err := h.Issues.SetStatusBulk(r.Context(), projectID, ids, status); err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
	}

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
			if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.HasPrefix(u.Path, "/\\") {
				return projectIssuesPath(projectID)
			}
			return u.RequestURI()
		}
	}
	return projectIssuesPath(projectID)
}
