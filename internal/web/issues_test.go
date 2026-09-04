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

// issuesStack — в отличие от newStack (auth_test.go), поднимает и PG, и CH:
// страницы issues читают event.Query.Sparklines, поэтому Events == nil здесь
// недопустим (в задаче 4 CH вообще не трогался).
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
	// Alerts/Uptime (задача 5, чек-лист «Первые шаги»): страница issues
	// определяет закрытые шаги онбординга по этим сервисам, поэтому стенд
	// заводит их так же, как newStack (auth_test.go) заводит h.Alerts.
	h.Alerts = alertSvc
	h.Uptime = uptimeSvc
	// Signals (аудит перед 1.0, K7-5/K7-6): отказы по ключу на пустом списке
	// issues и в чек-листе «Первые шаги» читают ту же таблицу, что пишет
	// Recorder на приёме — тесты бьют по ней напрямую через s.h.Signals.Bump.
	h.Signals = ingestsignal.NewStore(pool)
	h.Register(mux)

	return &issuesStack{pool: pool, srv: srv, h: h, org: orgSvc, auth: authSvc, issues: issueSvc, alerts: alertSvc, uptime: uptimeSvc, batcher: batcher}
}

// addEvent кладёт событие в батчер; для попадания в спарклайн теста нужен
// отдельный flushEvents, чтобы вставка в CH гарантированно завершилась до
// последующего GET.
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

// flushEvents синхронно доливает буфер батчера в CH (аналогично
// TestBatcherInsertsIntoClickHouse), не дожидаясь тикера. Close идемпотентен,
// поэтому повторный вызов в t.Cleanup после этого безопасен.
func (s *issuesStack) flushEvents(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.batcher.Close(ctx); err != nil {
		t.Fatalf("flush events: %v", err)
	}
}

// registerAndLogin — регистрирует нового юзера через auth.Service напрямую
// (без HTTP) и возвращает его id и cookie сессии для последующих запросов.
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

// createProject — организация + проект, владелец uid.
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

	// Issue 1: error, times_seen=1.
	r1, err := s.issues.Upsert(context.Background(), project.ID, "fp-error", "NullPointerException", "pkg/a.go:10", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue1: %v", err)
	}

	// Issue 2: warning, times_seen=3 (три Upsert увеличивают счётчик).
	var r2 issue.UpsertResult
	for i := 0; i < 3; i++ {
		r2, err = s.issues.Upsert(context.Background(), project.ID, "fp-warning", "Slow query detected", "pkg/b.go:20", "warning", "", now)
		if err != nil {
			t.Fatalf("upsert issue2: %v", err)
		}
	}

	// Issue 3: info, times_seen=1.
	r3, err := s.issues.Upsert(context.Background(), project.ID, "fp-info", "Deprecated API used", "pkg/c.go:30", "info", "", now)
	if err != nil {
		t.Fatalf("upsert issue3: %v", err)
	}

	// 2 события в CH для issue1 — должны попасть в спарклайн.
	s.addEvent(project.ID, r1.IssueID, now.Add(-2*time.Hour))
	s.addEvent(project.ID, r1.IssueID, now.Add(-1*time.Hour))
	s.flushEvents(t)

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"

	// GET списка → 200, содержит все 3 title и как минимум один <svg (спарклайн).
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

	// Компоновка непустого списка: тулбар с массовыми действиями стоит НАД
	// таблицей, кнопки вынесены из POST-формы и привязаны к ней атрибутом
	// form= (внутри тулбара лежат формы экспорта, вложенные <form> HTML не
	// допускает); старого блока .bulk-actions под таблицей больше нет.
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

	// Resolve issue1, затем ?status=resolved → только он.
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

	// ?q= фильтрует по подстроке title/culprit.
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

	// ?level=warning фильтрует по уровню.
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

	// Bulk resolve двух issues → 303, статусы поменялись.
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

	// POST bulk без same-origin Origin/Referer → 403.
	resp = postForm(t, s.srv, bulkPath, form, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", bulkPath, resp.StatusCode)
	}

	// Доступ чужим юзером (не участник организации) → 404.
	_, otherCookie := registerAndLogin(t, s, "issues-outsider@example.com")
	resp = getWithCookie(t, s.srv, issuesPath, otherCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", issuesPath, resp.StatusCode)
	}

	// POST bulk чужим юзером → тоже 404 (не должен видеть/трогать issues проекта).
	resp = postForm(t, s.srv, bulkPath, form, s.srv.URL, otherCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (outsider) status = %d, want 404", bulkPath, resp.StatusCode)
	}
}

// TestWebIssuesListHidesExportButtonsWhenExportsDisabled — на инстансе без
// каталога выгрузок (h.Exports == nil, дефолт newIssuesStack) кнопки
// «Выгрузить» на списке ошибок не должны рендериться вовсе: они вели бы на
// 404 (ревью веб-части E1, п.3). Включаем h.Exports обратно и проверяем, что
// кнопки появляются — гейт именно по h.Exports, не по чему-то ещё.
func TestWebIssuesListHidesExportButtonsWhenExportsDisabled(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-exports-owner@example.com")
	project := createProject(t, s, ownerID, "issues-exports-org", "issues-exports-proj")
	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"
	// Тулбар с кнопками экспорта рисуется только над непустым списком —
	// без хотя бы одной issue проверка гейта по h.Exports не отличима от
	// пустого состояния.
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

// TestWebIssuesGettingStartedChecklistFreshProject — задача 5 (docs-onboarding):
// свежий проект (нет событий/каналов/мониторов, в орге один участник —
// владелец) должен показывать карточку «Первые шаги» с прогрессом 1/4
// (шаг 1 «создать проект» уже закрыт) и CTA-ссылками на оставшиеся шаги.
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
	// CTA-ссылки на оставшиеся шаги (SDK/alerts/org settings).
	for _, href := range []string{
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/setup",
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/alerts",
	} {
		if !strings.Contains(string(body), href) {
			t.Fatalf("GET %s checklist missing CTA link %q: %s", issuesPath, href, body)
		}
	}
}

// TestWebIssuesGettingStartedChecklistAllDone — когда все 5 шагов онбординга
// закрыты (есть issue, есть канал алертов, в орге больше одного участника,
// добавлен монитор — шаги 4a/4b раздельные, №71), карточка «Первые шаги»
// больше не рендерится.
func TestWebIssuesGettingStartedChecklistAllDone(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "gs-done-owner@example.com")
	project := createProject(t, s, ownerID, "gs-done-org", "gs-done-proj")

	// Шаг 2: есть хотя бы одна issue (total > 0).
	if _, err := s.issues.Upsert(context.Background(), project.ID, "fp-done", "Boom", "pkg/a.go:1", "error", "", time.Now().UTC()); err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	// Шаг 3: есть канал доставки алертов.
	if _, err := s.alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: project.ID,
		Kind:      alert.ChannelEmail,
		Enabled:   true,
		Target:    "ops@example.com",
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// Шаг 4: в орге больше одного участника.
	memberID, _ := registerAndLogin(t, s, "gs-done-member@example.com")
	orgID, err := s.org.ProjectOrg(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("project org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), orgID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// Шаг 4b: добавлен монитор доступности.
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

// TestWebIssuesGettingStartedChecklistOperatorSees — C5: чек-лист «Первые
// шаги» гейтится на CanOperate, не CanManage, и оператор проекта (участник
// команды, role=member, без owner/admin) должен его видеть — с рабочими
// CTA на операторские шаги (SDK, алерт, монитор), но БЕЗ ссылки на шаг 4a
// «Позвать команду» (requireOrgRole — owner/admin only): рабочая ссылка
// увела бы оператора на честный 403, поэтому шаг остаётся как неактивный
// текст (gsStepReadOnly), не мёртвая ссылка.
func TestWebIssuesGettingStartedChecklistOperatorSees(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, _ := registerAndLogin(t, s, "gs-op-owner@example.com")
	project := createProject(t, s, ownerID, "gs-op-org", "gs-op-proj")
	orgID, err := s.org.ProjectOrg(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("project org: %v", err)
	}

	// Оператор: участник организации (role=member) на команде, привязанной
	// к проекту (тот же приём, что addTeamAccess в monitors_test.go, и
	// requireProjectOperator в operate_test.go) — RoleMember сам по себе
	// доступа к проекту не даёт (org.accessCondition), нужна команда.
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
	// Операторские шаги — рабочие CTA-ссылки.
	for _, href := range []string{
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/setup",
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/alerts",
		"/projects/" + strconv.FormatInt(project.ID, 10) + "/monitors",
	} {
		if !strings.Contains(string(body), `href="`+href+`"`) {
			t.Fatalf("GET %s (operator) checklist missing operator CTA link %q: %s", issuesPath, href, body)
		}
	}
	// Шаг 4a не должен вести на admin-only /orgs/{id}/settings — оператору
	// некуда там перейти. Здесь он неизбежно уже «сделан» (Step4aDone =
	// >1 участника в орге, а сам факт присоединения оператора уже даёт
	// второго участника), так что настоящую неактивную (gs-todo-locked)
	// отрисовку шага 4a при CanManage=false проверяет отдельный,
	// белоящичный тест шаблона — TestGettingStartedChecklistGatedByCanOperate
	// в internal/web/templates/pages_test.go: с реальным HTTP-флоу этого
	// сочетания (оператор есть, но участников всё ещё один) не бывает.
	orgSettingsHref := "/orgs/" + strconv.FormatInt(orgID, 10) + "/settings"
	if strings.Contains(string(body), `href="`+orgSettingsHref+`"`) {
		t.Fatalf("GET %s (operator) checklist should not link non-manageable step 4a to %q: %s", issuesPath, orgSettingsHref, body)
	}
}

// TestWebIssuesGettingStartedChecklistTeamlessMember404 — C5: участник
// организации без команды на проекте (role=member, не оператор) не должен
// видеть чек-лист — но не потому, что он спрятан отдельным условием, а
// потому что сама страница issues для него 404 (CanAccessProject), тот же
// existence-oracle принцип, что и у полного постороннего (см. тест выше по
// файлу) и у metricalerts_test.go:123.
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

// TestWebIssuesEnvironmentAndPeriodFilter проверяет ?env и ?period в списке
// issues: env сужает до issues с событиями в конкретном environment (по
// issue_environments), period отсекает issues со старым last_seen.
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

	// ?env=staging показывает только staging issue.
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

	// Подкручиваем last_seen prod issue на 2 суток назад, ?period=24h должен его отсечь.
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

// TestWebIssuesAssigneeColumn проверяет колонку Assignee: "—" без назначения,
// email назначенного юзера после Assign.
func TestWebIssuesAssigneeColumn(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-assignee-owner@example.com")
	project := createProject(t, s, ownerID, "issues-assignee-org", "issues-assignee-proj")

	// Assignee отдельно от owner: owner's email всегда в шапке страницы
	// (see the logout-form user-email span), так что проверка "email
	// появился только после назначения" требует другого адреса.
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

// TestWebIssuesPaginationPreservesFilters проверяет, что ссылки пагинации
// (Next) сохраняют env и period наряду со status/level/q/sort.
func TestWebIssuesPaginationPreservesFilters(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-pagefilter-owner@example.com")
	project := createProject(t, s, ownerID, "issues-pagefilter-org", "issues-pagefilter-proj")

	now := time.Now().UTC()
	// 26 issues в prod, чтобы default PerPage=25 дал вторую страницу.
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
	// Пагинация остаётся под таблицей: тулбар переехал наверх, а листалка — нет.
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

	// Test case 1: Protocol-relative path (same host as BaseURL) should be rejected
	req := &http.Request{
		Header: http.Header{
			"Referer": []string{"http://example.com//evil.com/x"},
		},
	}
	got := web.BulkRedirectTarget(req, baseURL, projectID)
	if got != expectedFallback {
		t.Errorf("protocol-relative referer: got %q, want %q", got, expectedFallback)
	}

	// Test case 1b: Backslash-prefixed path (browsers normalize "\" to "/",
	// turning "/\evil.com" into the same protocol-relative "//evil.com" as
	// test case 1) should also be rejected.
	reqBackslash := &http.Request{
		Header: http.Header{
			"Referer": []string{"http://example.com/\\evil.com"},
		},
	}
	gotBackslash := web.BulkRedirectTarget(reqBackslash, baseURL, projectID)
	if gotBackslash != expectedFallback {
		t.Errorf("backslash referer: got %q, want %q", gotBackslash, expectedFallback)
	}

	// Test case 2: Normal referer with path and query should be preserved
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

// TestWebIssuesFilteredEmptyState — пустой список различает «событий ещё не
// было» и «пусто из-за фильтров» (№23): при активном фильтре — свой текст и
// CTA «Сбросить фильтры» (ссылка на чистый список), без фильтра — прежний
// онбординговый текст с подключением DSN.
func TestWebIssuesFilteredEmptyState(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-filtered-empty@example.com")
	project := createProject(t, s, ownerID, "issues-fempty-org", "issues-fempty-proj")

	if _, err := s.issues.Upsert(context.Background(), project.ID, "fp-fe", "Prod NPE",
		"pkg/a.go:1", "error", "prod", time.Now().UTC()); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"

	// Фильтр, под который ничего не подходит → «ничего не подошло» + сброс.
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

	// Проект без единого события и без фильтров → прежний онбординговый текст.
	fresh := createProject(t, s, ownerID, "issues-fempty-org2", "issues-fempty-proj2")
	resp = getWithCookie(t, s.srv, "/projects/"+strconv.FormatInt(fresh.ID, 10)+"/issues", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Проблем пока нет") {
		t.Fatalf("чистый проект без фильтров не показал онбординговую пустоту: %s", body)
	}
}

// TestWebGettingStartedHide — «Скрыть» убирает чек-лист навсегда (№71): флаг
// живёт в профиле, а не в cookie, поэтому исчезает и после нового логина.
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

// TestWebIssuesListExportButtonsShowPIIOnlyForOwner — проверка боевой
// проводки issues.go (не только рендера templ, который уже покрыт
// TestIssuesListExportFormsGatePIIByCanManage в internal/web/templates):
// canManagePII, переданный в IssuesList, обязан быть настоящей ролью
// (owner/admin), а не, например, тем же значением, что canOperate (это и
// была бы незамеченная регрессия — оператор увидел бы галку include_pii,
// хотя бэкенд её для него игнорирует, exports.go:exportsCreate). Владелец
// (CanManage) видит галку include_pii на кнопках экспорта списка ошибок,
// оператор без CanManage (доступ только через команду) — нет, но сами
// кнопки (выбор формата) у него остаются.
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
	// Кнопки экспорта живут в тулбаре над таблицей, а он есть только у
	// непустого списка.
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

// TestWebIssuesEmptyStateShowsKeyRejects — K7-5/K7-6: проект без единой issue
// чаще всего означает не «событий ещё не было», а «SDK шлёт, но приём их
// отбраковывает» — неверный DSN, ключ чужого проекта или неподходящий тип.
// Отказ, случившийся в последний час, показывается прямо на пустом списке;
// отказ старше часа — уже не показывается (иначе баннер про "прямо сейчас"
// никогда бы не гас сам, даже после починки DSN); Signals == nil (стенд без
// per-project учёта, как в проде до этой правки) не должен ронять страницу —
// секция просто отсутствует.
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

	// M6: kind вне isKeyRejectKind (deprecated_logs — про устаревший адрес,
	// не про отказ по ключу) не должен породить эту врезку, даже свежий.
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

// TestWebGettingStartedChecklistShowsKeyRejects — тот же сигнал (K7-5/K7-6),
// но под шагом 2 чек-листа «Первые шаги»: пока SDK ещё не прислал ни одного
// события (шаг 2 не закрыт), отказ по ключу за последний час — самое частое
// объяснение почему, и должен быть виден рядом с CTA «Подключить».
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

// TestWebIssuesEmptyStateShowsKeyRejectsAfterGettingStartedHidden — G1
// (re-review аудита): чек-лист «Первые шаги» несёт ту же врезку об отказах
// по ключу, что и пустое состояние списка issues, и раньше пустое состояние
// показывало её только пока чек-лист виден (F5) — команда, скрывшая
// чек-лист кнопкой «Скрыть» (№71), не видела отказов по ключу нигде вовсе.
// Теперь скрытие чек-листа переносит врезку в пустое состояние, а не гасит
// её насовсем.
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
