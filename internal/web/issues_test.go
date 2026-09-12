package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// Поднимает PG и CH, в отличие от newStack: страницы issues читают event.Query.Sparklines,
// Events == nil здесь недопустим.
type issuesStack struct {
	pool    *pgxpool.Pool
	srv     *httptest.Server
	h       *web.Handler
	org     *org.Service
	auth    *auth.Service
	issues  *issue.Service
	alerts  *alert.Service
	uptime  *uptime.Service
	batcher *event.Batcher
}

func newIssuesStack(t *testing.T) *issuesStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	eventsQuery := event.NewQuery(ch)
	batcher := event.NewBatcher(ch)
	go batcher.Run()
	alertSvc := alert.NewService(pool)
	uptimeSvc := uptime.NewService(pool)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = batcher.Close(ctx)
	})

	h := web.New(authSvc, orgSvc, issueSvc, eventsQuery, srv.URL)
	// Alerts/Uptime заведены: страница issues по ним определяет закрытые шаги «Первые шаги».
	h.Alerts = alertSvc
	h.Uptime = uptimeSvc
	// Signals: тесты бьют по таблице отказов напрямую через s.h.Signals.Bump.
	h.Signals = ingestsignal.NewStore(pool)
	h.Register(mux)

	return &issuesStack{pool: pool, srv: srv, h: h, org: orgSvc, auth: authSvc, issues: issueSvc, alerts: alertSvc, uptime: uptimeSvc, batcher: batcher}
}

// Отдельный flushEvents нужен, чтобы вставка в CH завершилась до последующего GET.
func (s *issuesStack) addEvent(projectID, issueID int64, at time.Time) {
	s.batcher.Add(event.Event{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		IssueID:   issueID,
		Timestamp: at,
		Level:     "error",
		Message:   "boom",
		Tags:      map[string]string{},
	})
}

// Close идемпотентен, поэтому повторный вызов в t.Cleanup после этого безопасен.
func (s *issuesStack) flushEvents(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.batcher.Close(ctx); err != nil {
		t.Fatalf("flush events: %v", err)
	}
}

func registerAndLogin(t *testing.T, s *issuesStack, email string) (int64, *http.Cookie) {
	t.Helper()
	uid, err := s.auth.Register(context.Background(), email, "correct-horse-battery")
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	token, err := s.auth.CreateSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("create session for %s: %v", email, err)
	}
	return uid, &http.Cookie{Name: auth.CookieName, Value: token}
}

func createProject(t *testing.T, s *issuesStack, uid int64, orgSlug, projectSlug string) org.Project {
	t.Helper()
	o, err := s.org.CreateOrg(context.Background(), orgSlug, orgSlug, uid)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	p, err := s.org.CreateProject(context.Background(), o.ID, projectSlug, projectSlug, "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func getWithCookie(t *testing.T, srv *httptest.Server, path string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestWebIssuesList(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-owner@example.com")
	project := createProject(t, s, ownerID, "issues-org", "issues-proj")

	now := time.Now().UTC()

	r1, err := s.issues.Upsert(context.Background(), project.ID, "fp-error", "NullPointerException", "pkg/a.go:10", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue1: %v", err)
	}

	var r2 issue.UpsertResult
	for i := 0; i < 3; i++ {
		r2, err = s.issues.Upsert(context.Background(), project.ID, "fp-warning", "Slow query detected", "pkg/b.go:20", "warning", "", now)
		if err != nil {
			t.Fatalf("upsert issue2: %v", err)
		}
	}

	r3, err := s.issues.Upsert(context.Background(), project.ID, "fp-info", "Deprecated API used", "pkg/c.go:30", "info", "", now)
	if err != nil {
		t.Fatalf("upsert issue3: %v", err)
	}

	s.addEvent(project.ID, r1.IssueID, now.Add(-2*time.Hour))
	s.addEvent(project.ID, r1.IssueID, now.Add(-1*time.Hour))
	s.flushEvents(t)

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"

	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	for _, title := range []string{"NullPointerException", "Slow query detected", "Deprecated API used"} {
		if !strings.Contains(string(body), title) {
			t.Fatalf("GET %s body missing title %q: %s", issuesPath, title, body)
		}
	}
	if !strings.Contains(string(body), "<svg") {
		t.Fatalf("GET %s body missing <svg sparkline: %s", issuesPath, body)
	}

	html := string(body)
	if n := strings.Count(html, `id="issues-bulk"`); n != 1 {
		t.Fatalf("GET %s: id=\"issues-bulk\" встречается %d раз, want 1", issuesPath, n)
	}
	for _, action := range []string{"resolve", "ignore", "unresolve"} {
		btn := `form="issues-bulk" name="action" value="` + action + `"`
		if !strings.Contains(html, btn) {
			t.Errorf("GET %s: нет кнопки массового действия %s", issuesPath, btn)
		}
	}
	toolbarIdx := strings.Index(html, `class="card-toolbar"`)
	tableIdx := strings.Index(html, `<table class="data-table"`)
	if toolbarIdx < 0 || tableIdx < 0 {
		t.Fatalf("GET %s: нет тулбара (%d) или таблицы (%d)", issuesPath, toolbarIdx, tableIdx)
	}
	if toolbarIdx > tableIdx {
		t.Errorf("GET %s: тулбар (%d) должен стоять раньше таблицы (%d)", issuesPath, toolbarIdx, tableIdx)
	}
	if strings.Contains(html, "bulk-actions") {
		t.Errorf("GET %s: старый блок bulk-actions под таблицей должен исчезнуть", issuesPath)
	}

	if _, err := s.issues.SetStatusBulk(context.Background(), project.ID, []int64{r1.IssueID}, "resolved"); err != nil {
		t.Fatalf("set status bulk: %v", err)
	}
	resp = getWithCookie(t, s.srv, issuesPath+"?status=resolved", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?status=resolved status = %d, want 200", issuesPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "NullPointerException") {
		t.Fatalf("GET %s?status=resolved missing resolved issue: %s", issuesPath, body)
	}
	if strings.Contains(string(body), "Slow query detected") || strings.Contains(string(body), "Deprecated API used") {
		t.Fatalf("GET %s?status=resolved leaked non-resolved issues: %s", issuesPath, body)
	}

	resp = getWithCookie(t, s.srv, issuesPath+"?q=Slow", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?q=Slow status = %d, want 200", issuesPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "Slow query detected") {
		t.Fatalf("GET %s?q=Slow missing matching issue: %s", issuesPath, body)
	}
	if strings.Contains(string(body), "Deprecated API used") {
		t.Fatalf("GET %s?q=Slow leaked non-matching issue: %s", issuesPath, body)
	}

	resp = getWithCookie(t, s.srv, issuesPath+"?level=warning", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?level=warning status = %d, want 200", issuesPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "Slow query detected") {
		t.Fatalf("GET %s?level=warning missing matching issue: %s", issuesPath, body)
	}
	if strings.Contains(string(body), "Deprecated API used") || strings.Contains(string(body), "NullPointerException") {
		t.Fatalf("GET %s?level=warning leaked non-matching issue: %s", issuesPath, body)
	}

	bulkPath := issuesPath + "/bulk"
	form := url.Values{
		"action": {"resolve"},
		"ids":    {strconv.FormatInt(r2.IssueID, 10), strconv.FormatInt(r3.IssueID, 10)},
	}
	resp = postForm(t, s.srv, bulkPath, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", bulkPath, resp.StatusCode)
	}

	got2, err := s.issues.Get(context.Background(), r2.IssueID)
	if err != nil {
		t.Fatalf("get issue2: %v", err)
	}
	if got2.Status != "resolved" {
		t.Fatalf("issue2 status = %q, want resolved", got2.Status)
	}
	got3, err := s.issues.Get(context.Background(), r3.IssueID)
	if err != nil {
		t.Fatalf("get issue3: %v", err)
	}
	if got3.Status != "resolved" {
		t.Fatalf("issue3 status = %q, want resolved", got3.Status)
	}

	resp = postForm(t, s.srv, bulkPath, form, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", bulkPath, resp.StatusCode)
	}

	_, otherCookie := registerAndLogin(t, s, "issues-outsider@example.com")
	resp = getWithCookie(t, s.srv, issuesPath, otherCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", issuesPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, bulkPath, form, s.srv.URL, otherCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (outsider) status = %d, want 404", bulkPath, resp.StatusCode)
	}
}

func TestWebIssuesListHidesExportButtonsWhenExportsDisabled(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-exports-owner@example.com")
	project := createProject(t, s, ownerID, "issues-exports-org", "issues-exports-proj")
	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	// Тулбар с кнопками экспорта рисуется только над непустым списком — нужна хотя бы одна issue.
	if _, err := s.issues.Upsert(context.Background(), project.ID, "fp-exports", "Export me", "pkg/x.go:1", "error", "", time.Now().UTC()); err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), `action="/projects/`+strconv.FormatInt(project.ID, 10)+`/exports`) {
		t.Error("кнопки экспорта показаны при h.Exports == nil")
	}

	s.h.Exports = export.NewStore(s.pool)
	t.Cleanup(func() { s.h.Exports = nil })

	resp2 := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(body2), `action="/projects/`+strconv.FormatInt(project.ID, 10)+`/exports`) {
		t.Error("кнопки экспорта не показаны при включённом h.Exports")
	}
}

func TestWebIssuesGettingStartedChecklistFreshProject(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "gs-fresh-owner@example.com")
	project := createProject(t, s, ownerID, "gs-fresh-org", "gs-fresh-proj")

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `class="card getting-started"`) {
		t.Fatalf("GET %s missing getting-started checklist: %s", issuesPath, body)
	}
	if !strings.Contains(string(body), "1/5") {
		t.Fatalf("GET %s checklist missing 1/5 progress: %s", issuesPath, body)
	}
	for _, href := range []string{
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/setup",
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/alerts",
	} {
		if !strings.Contains(string(body), href) {
			t.Fatalf("GET %s checklist missing CTA link %q: %s", issuesPath, href, body)
		}
	}
}

func TestWebIssuesGettingStartedChecklistAllDone(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "gs-done-owner@example.com")
	project := createProject(t, s, ownerID, "gs-done-org", "gs-done-proj")

	if _, err := s.issues.Upsert(context.Background(), project.ID, "fp-done", "Boom", "pkg/a.go:1", "error", "", time.Now().UTC()); err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	if _, err := s.alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: project.ID,
		Kind:      alert.ChannelEmail,
		Enabled:   true,
		Target:    "ops@example.com",
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	memberID, _ := registerAndLogin(t, s, "gs-done-member@example.com")
	orgID, err := s.org.ProjectOrg(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("project org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), orgID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	if _, err := s.uptime.Create(context.Background(), uptime.Monitor{
		ProjectID: project.ID, Name: "gs-done-mon", Kind: uptime.KindHTTP, Enabled: true,
		IntervalSeconds: 60, TimeoutSeconds: 10, FailThreshold: 1, RecoveryThreshold: 1,
		Consensus: uptime.ConsensusMajority,
		Config:    monHTTPConfig(t, "https://example.com/health"),
	}, []string{"local"}, nil); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), `class="card getting-started"`) {
		t.Fatalf("GET %s should not show getting-started checklist when all steps are done: %s", issuesPath, body)
	}
}

// Чек-лист гейтится на CanOperate, не CanManage: оператор видит рабочие CTA, но шаг 4a
// «Позвать команду» (admin-only) — неактивный текст, не мёртвая ссылка на честный 403.
func TestWebIssuesGettingStartedChecklistOperatorSees(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, _ := registerAndLogin(t, s, "gs-op-owner@example.com")
	project := createProject(t, s, ownerID, "gs-op-org", "gs-op-proj")
	orgID, err := s.org.ProjectOrg(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("project org: %v", err)
	}

	// RoleMember сам по себе доступа к проекту не даёт (org.accessCondition) — нужна команда,
	// привязанная к проекту.
	operatorID, operatorCookie := registerAndLogin(t, s, "gs-op-operator@example.com")
	if err := s.org.AddMember(context.Background(), orgID, operatorID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	team, err := s.org.CreateTeam(context.Background(), orgID, "gs-op-team", "gs-op-team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.org.AddTeamMember(context.Background(), team.ID, operatorID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := s.org.AttachTeam(context.Background(), project.ID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	resp := getWithCookie(t, s.srv, issuesPath, operatorCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (operator) status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `class="card getting-started"`) {
		t.Fatalf("GET %s (operator) missing getting-started checklist: %s", issuesPath, body)
	}
	for _, href := range []string{
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/setup",
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/alerts",
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/monitors",
	} {
		if !strings.Contains(string(body), `href="`+href+`"`) {
			t.Fatalf("GET %s (operator) checklist missing operator CTA link %q: %s", issuesPath, href, body)
		}
	}
	orgSettingsHref := "/orgs/" + strconv.FormatInt(orgID, 10) + "/settings"
	if strings.Contains(string(body), `href="`+orgSettingsHref+`"`) {
		t.Fatalf("GET %s (operator) checklist should not link non-manageable step 4a to %q: %s", issuesPath, orgSettingsHref, body)
	}
}

// Не отдельным условием — сама страница issues 404 для него (CanAccessProject), тот же
// existence-oracle принцип, что и у постороннего.
func TestWebIssuesGettingStartedChecklistTeamlessMember404(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, _ := registerAndLogin(t, s, "gs-tl-owner@example.com")
	project := createProject(t, s, ownerID, "gs-tl-org", "gs-tl-proj")
	orgID, err := s.org.ProjectOrg(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("project org: %v", err)
	}

	memberID, memberCookie := registerAndLogin(t, s, "gs-tl-member@example.com")
	if err := s.org.AddMember(context.Background(), orgID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	resp := getWithCookie(t, s.srv, issuesPath, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (teamless member) status = %d, want 404: %s", issuesPath, resp.StatusCode, body)
	}
}

func TestWebIssuesEnvironmentAndPeriodFilter(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-envperiod-owner@example.com")
	project := createProject(t, s, ownerID, "issues-envperiod-org", "issues-envperiod-proj")

	now := time.Now().UTC()

	rProd, err := s.issues.Upsert(context.Background(), project.ID, "fp-web-prod", "Prod NPE", "pkg/a.go:1", "error", "prod", now)
	if err != nil {
		t.Fatalf("upsert prod: %v", err)
	}
	_, err = s.issues.Upsert(context.Background(), project.ID, "fp-web-staging", "Staging timeout", "pkg/b.go:2", "error", "staging", now)
	if err != nil {
		t.Fatalf("upsert staging: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"

	resp := getWithCookie(t, s.srv, issuesPath+"?env=staging", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?env=staging status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Staging timeout") {
		t.Fatalf("GET %s?env=staging missing staging issue: %s", issuesPath, body)
	}
	if strings.Contains(string(body), "Prod NPE") {
		t.Fatalf("GET %s?env=staging leaked prod issue: %s", issuesPath, body)
	}

	if _, err := s.pool.Exec(context.Background(), "UPDATE issues SET last_seen = $1 WHERE id = $2",
		now.Add(-48*time.Hour), rProd.IssueID); err != nil {
		t.Fatalf("backdate prod last_seen: %v", err)
	}
	resp = getWithCookie(t, s.srv, issuesPath+"?period=24h", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?period=24h status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Staging timeout") {
		t.Fatalf("GET %s?period=24h missing recent staging issue: %s", issuesPath, body)
	}
	if strings.Contains(string(body), "Prod NPE") {
		t.Fatalf("GET %s?period=24h leaked backdated prod issue: %s", issuesPath, body)
	}
}

func TestWebIssuesAssigneeColumn(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-assignee-owner@example.com")
	project := createProject(t, s, ownerID, "issues-assignee-org", "issues-assignee-proj")

	// Другой адрес: owner's email уже в шапке страницы, иначе «появился после назначения»
	// ничего не проверяла бы.
	assigneeID, _ := registerAndLogin(t, s, "issues-assignee-target@example.com")

	now := time.Now().UTC()
	r1, err := s.issues.Upsert(context.Background(), project.ID, "fp-web-assignee", "Needs owner", "pkg/a.go:1", "error", "", now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"

	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "—") {
		t.Fatalf("GET %s missing em-dash placeholder for unassigned issue: %s", issuesPath, body)
	}
	if strings.Contains(string(body), "issues-assignee-target@example.com") {
		t.Fatalf("GET %s shows assignee email before assignment: %s", issuesPath, body)
	}

	if err := s.issues.Assign(context.Background(), r1.IssueID, &assigneeID); err != nil {
		t.Fatalf("assign: %v", err)
	}
	resp = getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (after assign) status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "issues-assignee-target@example.com") {
		t.Fatalf("GET %s (after assign) missing assignee email: %s", issuesPath, body)
	}
}

func TestWebIssuesPaginationPreservesFilters(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-pagefilter-owner@example.com")
	project := createProject(t, s, ownerID, "issues-pagefilter-org", "issues-pagefilter-proj")

	now := time.Now().UTC()
	// PerPage=25: 26 issues дают вторую страницу.
	for i := 0; i < 26; i++ {
		fp := "fp-page-" + strconv.Itoa(i)
		if _, err := s.issues.Upsert(context.Background(), project.ID, fp, "Prod issue "+strconv.Itoa(i), "", "error", "prod", now); err != nil {
			t.Fatalf("upsert %s: %v", fp, err)
		}
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	resp := getWithCookie(t, s.srv, issuesPath+"?env=prod&period=24h", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?env=prod&period=24h status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "env=prod") || !strings.Contains(string(body), "period=24h") {
		t.Fatalf("GET %s?env=prod&period=24h pagination link missing filters: %s", issuesPath, body)
	}
	if !strings.Contains(string(body), "page=2") {
		t.Fatalf("GET %s?env=prod&period=24h missing next-page link: %s", issuesPath, body)
	}
	tableEnd := strings.Index(string(body), "</table>")
	pagerIdx := strings.Index(string(body), `class="pagination"`)
	if tableEnd < 0 || pagerIdx < 0 {
		t.Fatalf("GET %s: нет таблицы (%d) или пагинации (%d)", issuesPath, tableEnd, pagerIdx)
	}
	if pagerIdx < tableEnd {
		t.Errorf("GET %s: пагинация (%d) должна идти после таблицы (%d)", issuesPath, pagerIdx, tableEnd)
	}
}

func TestBulkRedirectTargetRejectsProtocolRelativePaths(t *testing.T) {
	baseURL := "http://example.com"
	projectID := int64(42)
	expectedFallback := "/projects/42/issues"

	req := &http.Request{
		Header: http.Header{
			"Referer": []string{"http://example.com//evil.com/x"},
		},
	}
	got := web.BulkRedirectTarget(req, baseURL, projectID)
	if got != expectedFallback {
		t.Errorf("protocol-relative referer: got %q, want %q", got, expectedFallback)
	}

	reqBackslash := &http.Request{
		Header: http.Header{
			"Referer": []string{"http://example.com/\\evil.com"},
		},
	}
	gotBackslash := web.BulkRedirectTarget(reqBackslash, baseURL, projectID)
	if gotBackslash != expectedFallback {
		t.Errorf("backslash referer: got %q, want %q", gotBackslash, expectedFallback)
	}

	req2 := &http.Request{
		Header: http.Header{
			"Referer": []string{"http://example.com/projects/42/issues?status=resolved&page=2"},
		},
	}
	got2 := web.BulkRedirectTarget(req2, baseURL, projectID)
	expected2 := "/projects/42/issues?status=resolved&page=2"
	if got2 != expected2 {
		t.Errorf("valid referer: got %q, want %q", got2, expected2)
	}
}

func TestWebIssuesFilteredEmptyState(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-filtered-empty@example.com")
	project := createProject(t, s, ownerID, "issues-fempty-org", "issues-fempty-proj")

	if _, err := s.issues.Upsert(context.Background(), project.ID, "fp-fe", "Prod NPE",
		"pkg/a.go:1", "error", "prod", time.Now().UTC()); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"

	resp := getWithCookie(t, s.srv, issuesPath+"?env=staging", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Ничего не подошло под фильтры") {
		t.Fatalf("активный фильтр без совпадений не показал filtered-текст: %s", body)
	}
	if !strings.Contains(string(body), "Сбросить фильтры") {
		t.Fatalf("нет CTA сброса фильтров: %s", body)
	}
	if strings.Contains(string(body), "Подключите DSN") {
		t.Fatalf("filtered-пустота показывает онбординговый текст: %s", body)
	}

	fresh := createProject(t, s, ownerID, "issues-fempty-org2", "issues-fempty-proj2")
	resp = getWithCookie(t, s.srv, "/projects/"+strconv.FormatInt(fresh.ID, 10)+"/issues", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Проблем пока нет") {
		t.Fatalf("чистый проект без фильтров не показал онбординговую пустоту: %s", body)
	}
}

// Флаг живёт в профиле, не в cookie — переживает новый логин.
func TestWebGettingStartedHide(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "gs-hide-owner@example.com")
	project := createProject(t, s, ownerID, "gs-hide-org", "gs-hide-proj")
	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"

	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "getting-started") {
		t.Fatalf("свежий проект без чек-листа: %s", body)
	}
	if !strings.Contains(string(body), "/profile/getting-started/hide") {
		t.Fatalf("на чек-листе нет кнопки «Скрыть»: %s", body)
	}

	resp = postForm(t, s.srv, "/profile/getting-started/hide", url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST hide status = %d, want 303", resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "getting-started") {
		t.Fatalf("чек-лист виден после скрытия: %s", body)
	}
}

// canManagePII должен быть настоящей ролью (owner/admin), не тем же значением, что canOperate —
// иначе оператор увидел бы галку include_pii, хотя бэкенд её для него игнорирует.
func TestWebIssuesListExportButtonsShowPIIOnlyForOwner(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "pii-owner@example.com")
	project := createProject(t, s, ownerID, "pii-org", "pii-proj")

	operatorID, operatorCookie := registerAndLogin(t, s, "pii-operator@example.com")
	if err := s.org.AddMember(context.Background(), project.OrgID, operatorID, org.RoleMember); err != nil {
		t.Fatalf("add operator as member: %v", err)
	}
	team, err := s.org.CreateTeam(context.Background(), project.OrgID, "pii-team", "pii-team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.org.AddTeamMember(context.Background(), team.ID, operatorID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := s.org.AttachTeam(context.Background(), project.ID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	s.h.Exports = export.NewStore(s.pool)
	t.Cleanup(func() { s.h.Exports = nil })

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	if _, err := s.issues.Upsert(context.Background(), project.ID, "fp-pii", "PII issue", "pkg/x.go:1", "error", "", time.Now().UTC()); err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	ownerBody := readAll(t, getWithCookie(t, s.srv, issuesPath, ownerCookie))
	if n := strings.Count(ownerBody, `name="include_pii"`); n != 2 {
		t.Errorf("владельцу показано %d галок include_pii, want 2 (группы + события): %s", n, ownerBody)
	}

	operatorBody := readAll(t, getWithCookie(t, s.srv, issuesPath, operatorCookie))
	if strings.Contains(operatorBody, `name="include_pii"`) {
		t.Error("оператору без CanManage показана галка include_pii на списке ошибок")
	}
	if n := strings.Count(operatorBody, `<select name="format"`); n != 2 {
		t.Errorf("оператору должны остаться обе кнопки экспорта с выбором формата, селекторов format = %d, want 2", n)
	}
}

// Отказ последнего часа показывается на пустом списке; старше часа — уже нет, иначе баннер
// не гас бы даже после починки DSN.
func TestWebIssuesEmptyStateShowsKeyRejects(t *testing.T) {
	s := newIssuesStack(t)
	ctx := context.Background()

	ownerID, ownerCookie := registerAndLogin(t, s, "kr-owner@example.com")

	fresh := createProject(t, s, ownerID, "kr-fresh-org", "kr-fresh-proj")
	if err := s.h.Signals.Bump(ctx, fresh.ID, ingestsignal.KindKeyInvalid, 7, time.Now()); err != nil {
		t.Fatalf("bump fresh: %v", err)
	}
	freshPath := "/projects/" + strconv.FormatInt(fresh.ID, 10) + "/issues"
	resp := getWithCookie(t, s.srv, freshPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", freshPath, resp.StatusCode, body)
	}
	wantReason := i18n.Tf(ctx, "ingest_signals.rejects.key_invalid", "hits", "7")
	if !strings.Contains(string(body), wantReason) {
		t.Errorf("GET %s missing key-reject reason %q: %s", freshPath, wantReason, body)
	}
	settingsPath := "/projects/" + strconv.FormatInt(fresh.ID, 10) + "/settings"
	if !strings.Contains(string(body), settingsPath) {
		t.Errorf("GET %s missing link to project settings %q: %s", freshPath, settingsPath, body)
	}

	stale := createProject(t, s, ownerID, "kr-stale-org", "kr-stale-proj")
	if err := s.h.Signals.Bump(ctx, stale.ID, ingestsignal.KindKeyInvalid, 4, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("bump stale: %v", err)
	}
	stalePath := "/projects/" + strconv.FormatInt(stale.ID, 10) + "/issues"
	resp = getWithCookie(t, s.srv, stalePath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", stalePath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), i18n.T(ctx, "ingest_signals.rejects.title")) {
		t.Errorf("GET %s shows key-reject notice for a rejection older than 1 hour: %s", stalePath, body)
	}

	noSignals := createProject(t, s, ownerID, "kr-nosig-org", "kr-nosig-proj")
	prevSignals := s.h.Signals
	s.h.Signals = nil
	t.Cleanup(func() { s.h.Signals = prevSignals })
	noSignalsPath := "/projects/" + strconv.FormatInt(noSignals.ID, 10) + "/issues"
	resp = getWithCookie(t, s.srv, noSignalsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (Signals=nil) status = %d, want 200: %s", noSignalsPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), i18n.T(ctx, "ingest_signals.rejects.title")) {
		t.Errorf("GET %s (Signals=nil) unexpectedly shows key-reject notice: %s", noSignalsPath, body)
	}

	// kind вне isKeyRejectKind (напр. deprecated_logs) не должен породить эту врезку.
	s.h.Signals = prevSignals
	wrongKind := createProject(t, s, ownerID, "kr-wrongkind-org", "kr-wrongkind-proj")
	if err := s.h.Signals.Bump(ctx, wrongKind.ID, ingestsignal.KindDeprecatedLogs, 6, time.Now()); err != nil {
		t.Fatalf("bump wrongkind: %v", err)
	}
	wrongKindPath := "/projects/" + strconv.FormatInt(wrongKind.ID, 10) + "/issues"
	resp = getWithCookie(t, s.srv, wrongKindPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", wrongKindPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), i18n.T(ctx, "ingest_signals.rejects.title")) {
		t.Errorf("GET %s shows key-reject notice for a deprecated-path signal (kind outside isKeyRejectKind): %s", wrongKindPath, body)
	}
}

func TestWebGettingStartedChecklistShowsKeyRejects(t *testing.T) {
	s := newIssuesStack(t)
	ctx := context.Background()

	ownerID, ownerCookie := registerAndLogin(t, s, "kr-gs-owner@example.com")
	project := createProject(t, s, ownerID, "kr-gs-org", "kr-gs-proj")
	if err := s.h.Signals.Bump(ctx, project.ID, ingestsignal.KindKeyScope, 2, time.Now()); err != nil {
		t.Fatalf("bump: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `class="card getting-started"`) {
		t.Fatalf("GET %s missing getting-started checklist: %s", issuesPath, body)
	}
	wantReason := i18n.Tf(ctx, "ingest_signals.rejects.key_scope", "hits", "2")
	if !strings.Contains(string(body), wantReason) {
		t.Errorf("GET %s checklist missing key-reject reason %q: %s", issuesPath, wantReason, body)
	}
}

// Скрытие чек-листа переносит врезку об отказах по ключу в пустое состояние списка, не гасит её.
func TestWebIssuesEmptyStateShowsKeyRejectsAfterGettingStartedHidden(t *testing.T) {
	s := newIssuesStack(t)
	ctx := context.Background()

	ownerID, ownerCookie := registerAndLogin(t, s, "kr-hidden-owner@example.com")
	project := createProject(t, s, ownerID, "kr-hidden-org", "kr-hidden-proj")

	resp := postForm(t, s.srv, "/profile/getting-started/hide", url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST hide status = %d, want 303", resp.StatusCode)
	}

	if err := s.h.Signals.Bump(ctx, project.ID, ingestsignal.KindKeyInvalid, 5, time.Now()); err != nil {
		t.Fatalf("bump: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	resp = getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), `class="card getting-started"`) {
		t.Fatalf("GET %s should not show the hidden getting-started checklist: %s", issuesPath, body)
	}
	if !strings.Contains(string(body), `class="notice notice--warn"`) {
		t.Fatalf("GET %s missing the empty-state key-reject notice once the checklist is hidden: %s", issuesPath, body)
	}
	wantReason := i18n.Tf(ctx, "ingest_signals.rejects.key_invalid", "hits", "5")
	if !strings.Contains(string(body), wantReason) {
		t.Errorf("GET %s empty-state notice missing key-reject reason %q: %s", issuesPath, wantReason, body)
	}
}

// canAccess и canOperateProject читают org_members.role тем же текстом запроса — ломаем её
// и проверяем, что ошибка всплывает уже на canAccess (гейт всей страницы), не где-то глубже.
func TestWebIssuesCanAccessProjectQueryError(t *testing.T) {
	s := newIssuesStack(t)
	ctx := context.Background()
	uid, cookie := registerAndLogin(t, s, "issues-accesserr@example.com")
	project := createProject(t, s, uid, "issues-accesserr-org", "issues-accesserr-proj")

	if _, err := s.pool.Exec(ctx, "ALTER TABLE org_members RENAME COLUMN role TO role_broken_for_test"); err != nil {
		t.Fatalf("break org_members.role: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	resp := getWithCookie(t, s.srv, issuesPath, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GET %s с поломанной БД: status = %d, want 500 (не 403/404 — отказ проверки прав, не отказ в правах): %s",
			issuesPath, resp.StatusCode, body)
	}
}
