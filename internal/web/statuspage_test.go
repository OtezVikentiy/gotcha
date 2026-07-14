package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// statusPageStack — стенд публичной статус-страницы и её настроек: как
// monitorsStack (PG + CH: странице нужны Bars/Uptime из ClickHouse), но с
// собственными хелперами создания проекта и мониторов.
type statusPageStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	uptime *uptime.Service
	writer *uptime.ResultWriter
}

func newStatusPageStack(t *testing.T) *statusPageStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query

	uptimeSvc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	h.Uptime = uptimeSvc
	h.UptimeWriter = writer
	h.UptimeQuery = uptime.NewQuery(ch)
	h.Register(mux)

	return &statusPageStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, uptime: uptimeSvc, writer: writer}
}

func (s *statusPageStack) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.writer.Close(ctx); err != nil {
		t.Fatalf("flush writer: %v", err)
	}
}

// statusPageProject — организация с owner/admin/member и проектом, к которому
// member имеет доступ только на просмотр (через команду).
func statusPageProject(t *testing.T, s *statusPageStack, prefix string) (org.Project, *http.Cookie, *http.Cookie) {
	t.Helper()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, prefix+"-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, prefix+"-member@example.com")

	o, err := s.org.CreateOrg(context.Background(), prefix+"-co", prefix+" Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, prefix+"-proj", prefix+" Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, s.org, o.ID, proj.ID, memberID, prefix+"-team")
	return proj, ownerCookie, memberCookie
}

// statusPageMonitor создаёт HTTP-монитор с URL, которого НЕ должно быть в
// публичном HTML (см. TestWebStatusPagePublicHidesInternals).
func statusPageMonitor(t *testing.T, s *statusPageStack, projectID int64, name, target string) uptime.Monitor {
	t.Helper()
	m := baseMonitor(projectID, name)
	m.Config = monHTTPConfig(t, target)
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor %s: %v", name, err)
	}
	return created
}

func getAnon(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp := getWithCookie(t, srv, path, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

// TestWebStatusPagePublicHidesInternals — аноним видит страницу (200) с
// display_name мониторов и SVG-полоской, но НИ ОДНОГО внутреннего факта:
// ни URL монитора, ни его настоящего имени, ни текста последней ошибки.
func TestWebStatusPagePublicHidesInternals(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "sppublic")

	api := statusPageMonitor(t, s, proj.ID, "checkout-api-prod", "https://example.com/health")
	db := statusPageMonitor(t, s, proj.ID, "billing-db-primary", "https://example.com/billing")

	at := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := s.uptime.ApplyResult(context.Background(), api.ID, "local", true, "", at); err != nil {
		t.Fatalf("apply result: %v", err)
	}
	if _, err := s.uptime.ApplyResult(context.Background(), db.ID, "local", true, "", at); err != nil {
		t.Fatalf("apply result: %v", err)
	}
	s.writer.Add(proj.ID, api.ID, "local", at, uptime.Result{OK: true, StatusCode: 200, TotalMs: 100})
	s.writer.Add(proj.ID, db.ID, "local", at, uptime.Result{OK: true, StatusCode: 200, TotalMs: 90})
	s.flush(t)

	if _, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "sppublic-status", Title: "Acme Status", Description: "Состояние наших сервисов", Enabled: true,
	}, []uptime.StatusPageMonitor{
		{MonitorID: api.ID, DisplayName: "API", Position: 0},
		{MonitorID: db.ID, DisplayName: "Billing", Position: 1},
	}); err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, body := getAnon(t, s.srv, "/status/sppublic-status")
	if status != http.StatusOK {
		t.Fatalf("GET /status/sppublic-status (anon) = %d, want 200: %s", status, body)
	}
	for _, want := range []string{"Acme Status", "Состояние наших сервисов", "API", "Billing", "<svg", "All systems operational"} {
		if !strings.Contains(body, want) {
			t.Fatalf("public status page missing %q: %s", want, body)
		}
	}
	// Утечки внутренностей: URL монитора, его настоящее имя, имя проекта и
	// организации не должны попадать в публичный HTML.
	for _, leak := range []string{"example.com", "checkout-api-prod", "billing-db-primary", "sppublic-proj", "sppublic Proj", "/projects/", "/monitors/"} {
		if strings.Contains(body, leak) {
			t.Fatalf("public status page leaks %q: %s", leak, body)
		}
	}
}

// TestWebStatusPagePartialOutage — один из двух мониторов в down: общий
// статус «Partial outage», а текст ошибки (в нём хост/IP) не рендерится.
func TestWebStatusPagePartialOutage(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "sppartial")

	up := statusPageMonitor(t, s, proj.ID, "web-front", "https://example.com/")
	down := statusPageMonitor(t, s, proj.ID, "db-primary", "https://example.com/db")

	at := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := s.uptime.ApplyResult(context.Background(), up.ID, "local", true, "", at); err != nil {
		t.Fatalf("apply result: %v", err)
	}
	if _, err := s.uptime.ApplyResult(context.Background(), down.ID, "local", false, "dial tcp 10.0.0.5:5432: connection refused", at); err != nil {
		t.Fatalf("apply result: %v", err)
	}
	if _, _, err := s.uptime.OpenIncident(context.Background(), down.ID, "dial tcp 10.0.0.5:5432: connection refused", []string{"local"}, false); err != nil {
		t.Fatalf("open incident: %v", err)
	}

	if _, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "sppartial-status", Title: "Partial", Enabled: true,
	}, []uptime.StatusPageMonitor{
		{MonitorID: up.ID, DisplayName: "Website", Position: 0},
		{MonitorID: down.ID, DisplayName: "Database", Position: 1},
	}); err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, body := getAnon(t, s.srv, "/status/sppartial-status")
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", status, body)
	}
	if !strings.Contains(body, "Partial outage") {
		t.Fatalf("want «Partial outage» with one monitor down: %s", body)
	}
	for _, leak := range []string{"10.0.0.5", "connection refused", "example.com", "db-primary", "local"} {
		if strings.Contains(body, leak) {
			t.Fatalf("public status page leaks %q: %s", leak, body)
		}
	}
}

// TestWebStatusPageDisabledAndUnknown404 — выключенная страница и
// несуществующий slug дают одинаковую 404; отрицательный ответ не кешируется
// (создав страницу с тем же slug, тут же получаем 200).
func TestWebStatusPageDisabledAndUnknown404(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spoff")

	m := statusPageMonitor(t, s, proj.ID, "hidden-monitor", "https://example.com/hidden")
	if _, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "spoff-disabled", Title: "Disabled", Enabled: false,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}}); err != nil {
		t.Fatalf("create status page: %v", err)
	}

	if status, body := getAnon(t, s.srv, "/status/spoff-disabled"); status != http.StatusNotFound {
		t.Fatalf("GET disabled page = %d, want 404: %s", status, body)
	}
	if status, body := getAnon(t, s.srv, "/status/spoff-missing"); status != http.StatusNotFound {
		t.Fatalf("GET unknown slug = %d, want 404: %s", status, body)
	}

	// 404 не кешируется: страница, созданная сразу после промаха, видна.
	if _, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "spoff-missing", Title: "Now Exists", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}}); err != nil {
		t.Fatalf("create status page: %v", err)
	}
	status, body := getAnon(t, s.srv, "/status/spoff-missing")
	if status != http.StatusOK {
		t.Fatalf("GET after create = %d, want 200 (404 must not be cached): %s", status, body)
	}
	if !strings.Contains(body, "Now Exists") {
		t.Fatalf("want fresh page content: %s", body)
	}
}

// TestWebStatusPageCached — успешный ответ кешируется на 30 секунд: правка
// display_name в БД сразу после первого запроса не видна во втором (тот же
// байт-в-байт HTML), то есть повторный запрос не ходил ни в PG, ни в CH.
func TestWebStatusPageCached(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spcache")

	m := statusPageMonitor(t, s, proj.ID, "cached-monitor", "https://example.com/cached")
	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "spcache-status", Title: "Cached", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Old Name", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, first := getAnon(t, s.srv, "/status/spcache-status")
	if status != http.StatusOK || !strings.Contains(first, "Old Name") {
		t.Fatalf("first GET = %d, want 200 with «Old Name»: %s", status, first)
	}

	sp.Title = "Renamed"
	if err := s.uptime.UpdateStatusPage(context.Background(), sp,
		[]uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "New Name", Position: 0}}); err != nil {
		t.Fatalf("update status page: %v", err)
	}

	status, second := getAnon(t, s.srv, "/status/spcache-status")
	if status != http.StatusOK {
		t.Fatalf("second GET = %d, want 200", status)
	}
	if second != first {
		t.Fatalf("second response differs from the first — cache miss within 30s:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if strings.Contains(second, "New Name") || strings.Contains(second, "Renamed") {
		t.Fatalf("cached response must not reflect the update: %s", second)
	}
}

// statusPageStampedeRequests — сколько параллельных анонимных запросов бьёт в
// холодный slug в TestWebStatusPageStampede.
const statusPageStampedeRequests = 24

// TestWebStatusPageStampede — на холодном (или только что протухшем) slug'е
// страницу собирает РОВНО ОДИН запрос, остальные ждут его результат: публичный
// роут без аутентификации, и без single-flight аноним с десятком параллельных
// соединений множил бы на десять всю сборку (~5 запросов в PG/CH на каждый
// монитор) каждые 30 секунд.
//
// Сборки считаются по числу обращений к пулу PG (pgxpool.Stat().AcquireCount —
// сборка ходит в PG на каждом шаге, а на этот роут анонимом больше никто в PG
// не ходит): сначала меряем цену ровно одной сборки на «прогревочной» странице
// той же формы, потом стучимся statusPageStampedeRequests раз параллельно в
// холодную. Без single-flight цена вырастет примерно в 24 раза.
func TestWebStatusPageStampede(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spflight")

	var monitors []uptime.StatusPageMonitor
	for i := range 3 {
		m := statusPageMonitor(t, s, proj.ID, "flight-monitor-"+strconv.Itoa(i), "https://example.com/flight")
		monitors = append(monitors, uptime.StatusPageMonitor{
			MonitorID: m.ID, DisplayName: "Service " + strconv.Itoa(i), Position: i,
		})
	}

	// Две страницы одной формы: warm — эталон цены одной сборки, cold —
	// мишень штурма.
	for _, slug := range []string{"spflight-warm", "spflight-cold"} {
		if _, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
			ProjectID: proj.ID, Slug: slug, Title: "Flight " + slug, Enabled: true,
		}, monitors); err != nil {
			t.Fatalf("create status page %s: %v", slug, err)
		}
	}

	before := s.pool.Stat().AcquireCount()
	if status, body := getAnon(t, s.srv, "/status/spflight-warm"); status != http.StatusOK {
		t.Fatalf("warm-up GET = %d, want 200: %s", status, body)
	}
	oneBuild := s.pool.Stat().AcquireCount() - before
	if oneBuild == 0 {
		t.Fatalf("single build made no PG queries — the counter is not measuring builds")
	}

	before = s.pool.Stat().AcquireCount()

	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make([]int, statusPageStampedeRequests)
	bodies := make([]string, statusPageStampedeRequests)
	for i := range statusPageStampedeRequests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Get(s.srv.URL + "/status/spflight-cold")
			if err != nil {
				statuses[i] = -1
				bodies[i] = err.Error()
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			statuses[i] = resp.StatusCode
			bodies[i] = string(raw)
		}()
	}
	close(start)
	wg.Wait()

	// Все запросы получили корректную страницу.
	for i := range statusPageStampedeRequests {
		if statuses[i] != http.StatusOK {
			t.Fatalf("concurrent GET #%d = %d, want 200: %s", i, statuses[i], bodies[i])
		}
		if !strings.Contains(bodies[i], "Flight spflight-cold") || !strings.Contains(bodies[i], "Service 0") {
			t.Fatalf("concurrent GET #%d returned an incomplete page: %s", i, bodies[i])
		}
	}

	// ...и собрана она была один раз: запас в 2× покрывает служебный шум пула,
	// но и близко не покрывает 24 независимых сборки.
	spent := s.pool.Stat().AcquireCount() - before
	if spent > 2*oneBuild {
		t.Fatalf("%d concurrent requests spent %d PG acquires (one build = %d): the cache is not single-flight",
			statusPageStampedeRequests, spent, oneBuild)
	}
}

// TestWebStatusPagesForeignMonitorRejected — монитор ЧУЖОГО проекта, отправленный
// в форму создания статус-страницы, не привязывается к ней и не появляется на
// публичной странице (граница проекта: parseStatusPageForm принимает только
// мониторы своего проекта).
func TestWebStatusPagesForeignMonitorRejected(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, _ := statusPageProject(t, s, "spforeign")
	otherProj, _, _ := statusPageProject(t, s, "spvictim")

	mine := statusPageMonitor(t, s, proj.ID, "own-monitor", "https://example.com/own")
	foreign := statusPageMonitor(t, s, otherProj.ID, "victim-monitor", "https://example.com/victim")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/statuspages"
	form := url.Values{
		"slug":     {"spforeign-status"},
		"title":    {"Foreign"},
		"enabled":  {"on"},
		"monitors": {strconv.FormatInt(mine.ID, 10), strconv.FormatInt(foreign.ID, 10)},
		"display_name_" + strconv.FormatInt(mine.ID, 10):    {"Mine"},
		"display_name_" + strconv.FormatInt(foreign.ID, 10): {"Stolen"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s = %d, want 303: %s", path, resp.StatusCode, body)
	}

	pages, err := s.uptime.StatusPagesOf(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("status pages of: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("len(pages) = %d, want 1", len(pages))
	}
	attached, err := s.uptime.StatusPageMonitors(context.Background(), pages[0].ID)
	if err != nil {
		t.Fatalf("status page monitors: %v", err)
	}
	if len(attached) != 1 || attached[0].MonitorID != mine.ID {
		t.Fatalf("attached = %+v, want only the own monitor %d (foreign %d must be dropped)",
			attached, mine.ID, foreign.ID)
	}

	status, pub := getAnon(t, s.srv, "/status/spforeign-status")
	if status != http.StatusOK {
		t.Fatalf("GET public page = %d, want 200: %s", status, pub)
	}
	if !strings.Contains(pub, "Mine") {
		t.Fatalf("public page must show the own monitor: %s", pub)
	}
	for _, leak := range []string{"Stolen", "victim-monitor"} {
		if strings.Contains(pub, leak) {
			t.Fatalf("public page shows a monitor of another project (%q): %s", leak, pub)
		}
	}
}

// TestWebStatusPagesForeignOrigin — все три POST'а настроек статус-страниц
// закрыты sameOrigin: запрос с чужим Origin (CSRF) отвергается 403 и ничего не
// меняет.
func TestWebStatusPagesForeignOrigin(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, _ := statusPageProject(t, s, "sporigin")
	m := statusPageMonitor(t, s, proj.ID, "origin-monitor", "https://example.com/origin")

	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "sporigin-status", Title: "Origin", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	const evil = "https://evil.example.com"
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/statuspages"
	spPath := "/statuspages/" + strconv.FormatInt(sp.ID, 10)

	cases := []struct {
		path string
		form url.Values
	}{
		{path, url.Values{"slug": {"sporigin-new"}, "title": {"New"}, "enabled": {"on"}}},
		{spPath, url.Values{"slug": {"sporigin-status"}, "title": {"Hacked"}, "enabled": {"on"}}},
		{spPath + "/delete", url.Values{}},
	}
	for _, c := range cases {
		resp := postForm(t, s.srv, c.path, c.form, evil, ownerCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s (foreign origin) = %d, want 403", c.path, resp.StatusCode)
		}
	}

	pages, err := s.uptime.StatusPagesOf(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("status pages of: %v", err)
	}
	if len(pages) != 1 || pages[0].Title != "Origin" {
		t.Fatalf("pages = %+v, want the single untouched «Origin» page (cross-origin POSTs must not persist)", pages)
	}
}

// TestWebStatusPagesSettingsCRUD — admin создаёт страницу, видит публичный
// URL, ловит 422 на занятый slug (с сохранением введённых значений), правит
// display_name и удаляет страницу.
func TestWebStatusPagesSettingsCRUD(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, _ := statusPageProject(t, s, "spcrud")
	m := statusPageMonitor(t, s, proj.ID, "crud-monitor", "https://example.com/crud")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/statuspages"

	// GET: форма создания со списком мониторов проекта.
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "crud-monitor") {
		t.Fatalf("settings page must list project monitors: %s", body)
	}

	form := url.Values{
		"slug":                                        {"spcrud-status"},
		"title":                                       {"CRUD Status"},
		"description":                                 {"desc"},
		"enabled":                                     {"on"},
		"monitors":                                    {strconv.FormatInt(m.ID, 10)},
		"display_name_" + strconv.FormatInt(m.ID, 10): {"Public API"},
	}
	resp = postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s = %d, want 303: %s", path, resp.StatusCode, body)
	}

	pages, err := s.uptime.StatusPagesOf(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("status pages of: %v", err)
	}
	if len(pages) != 1 || pages[0].Slug != "spcrud-status" || !pages[0].Enabled {
		t.Fatalf("pages = %+v, want single enabled spcrud-status", pages)
	}
	pageID := pages[0].ID

	// GET: ссылка на публичный URL и текущий display_name в форме
	// редактирования.
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), s.srv.URL+"/status/spcrud-status") {
		t.Fatalf("settings page must show the public URL: %s", body)
	}
	if !strings.Contains(string(body), "Public API") {
		t.Fatalf("edit form must prefill display_name: %s", body)
	}

	// Занятый slug -> 422, введённые значения сохранены, вторая страница не
	// создана.
	taken := url.Values{
		"slug":     {"spcrud-status"},
		"title":    {"Another Status"},
		"enabled":  {"on"},
		"monitors": {strconv.FormatInt(m.ID, 10)},
	}
	resp = postForm(t, s.srv, path, taken, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (taken slug) = %d, want 422: %s", path, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Another Status") {
		t.Fatalf("422 must re-render the submitted values: %s", body)
	}
	pages, err = s.uptime.StatusPagesOf(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("status pages of: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("len(pages) = %d, want 1 (422 must not persist)", len(pages))
	}

	// Update: новый display_name виден на публичной странице.
	updatePath := "/statuspages/" + strconv.FormatInt(pageID, 10)
	update := url.Values{
		"slug":                                        {"spcrud-status"},
		"title":                                       {"CRUD Status"},
		"enabled":                                     {"on"},
		"monitors":                                    {strconv.FormatInt(m.ID, 10)},
		"display_name_" + strconv.FormatInt(m.ID, 10): {"Renamed API"},
	}
	resp = postForm(t, s.srv, updatePath, update, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s = %d, want 303: %s", updatePath, resp.StatusCode, body)
	}
	status, pub := getAnon(t, s.srv, "/status/spcrud-status")
	if status != http.StatusOK || !strings.Contains(pub, "Renamed API") {
		t.Fatalf("public page after update = %d: %s", status, pub)
	}

	// Delete.
	resp = postForm(t, s.srv, updatePath+"/delete", url.Values{}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST delete = %d, want 303: %s", resp.StatusCode, body)
	}
	pages, err = s.uptime.StatusPagesOf(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("status pages of: %v", err)
	}
	if len(pages) != 0 {
		t.Fatalf("len(pages) = %d, want 0 after delete", len(pages))
	}
}

// TestWebStatusPagesSettingsMemberForbidden — member (доступ к проекту только
// на просмотр) получает 404 на настройках, и чужая статус-страница по прямому
// id тоже 404.
func TestWebStatusPagesSettingsMemberForbidden(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, memberCookie := statusPageProject(t, s, "spforbid")
	_, otherOwnerCookie, _ := statusPageProject(t, s, "spother")

	m := statusPageMonitor(t, s, proj.ID, "forbid-monitor", "https://example.com/forbid")
	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Slug: "spforbid-status", Title: "Forbid", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/statuspages"

	resp := getWithCookie(t, s.srv, path, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) = %d, want 404", path, resp.StatusCode)
	}

	resp = postForm(t, s.srv, path, url.Values{"slug": {"x"}, "title": {"x"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) = %d, want 404", path, resp.StatusCode)
	}

	updatePath := "/statuspages/" + strconv.FormatInt(sp.ID, 10)
	resp = postForm(t, s.srv, updatePath, url.Values{"slug": {"spforbid-status"}, "title": {"Hacked"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) = %d, want 404", updatePath, resp.StatusCode)
	}

	// Owner другой организации не должен трогать чужую страницу по её id.
	resp = postForm(t, s.srv, updatePath+"/delete", url.Values{}, s.srv.URL, otherOwnerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s/delete (foreign owner) = %d, want 404", updatePath, resp.StatusCode)
	}

	// Sanity: owner проекта всё ещё видит настройки.
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) = %d, want 200", path, resp.StatusCode)
	}

	pages, err := s.uptime.StatusPagesOf(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("status pages of: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("len(pages) = %d, want 1 (foreign/member writes must not persist)", len(pages))
	}
}
