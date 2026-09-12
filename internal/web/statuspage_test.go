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

	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Acme Status", Description: "Состояние наших сервисов", Enabled: true,
	}, []uptime.StatusPageMonitor{
		{MonitorID: api.ID, DisplayName: "API", Position: 0},
		{MonitorID: db.ID, DisplayName: "Billing", Position: 1},
	})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, body := getAnon(t, s.srv, "/status/"+sp.PublicID)
	if status != http.StatusOK {
		t.Fatalf("GET /status/%s (anon) = %d, want 200: %s", sp.PublicID, status, body)
	}
	for _, want := range []string{"Acme Status", "Состояние наших сервисов", "API", "Billing", "<svg", "Все системы работают"} {
		if !strings.Contains(body, want) {
			t.Fatalf("public status page missing %q: %s", want, body)
		}
	}
	for _, leak := range []string{"example.com", "checkout-api-prod", "billing-db-primary", "sppublic-proj", "sppublic Proj", "/projects/", "/monitors/"} {
		if strings.Contains(body, leak) {
			t.Fatalf("public status page leaks %q: %s", leak, body)
		}
	}
}

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

	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Partial", Enabled: true,
	}, []uptime.StatusPageMonitor{
		{MonitorID: up.ID, DisplayName: "Website", Position: 0},
		{MonitorID: down.ID, DisplayName: "Database", Position: 1},
	})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, body := getAnon(t, s.srv, "/status/"+sp.PublicID)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", status, body)
	}
	if !strings.Contains(body, "Частичный сбой") {
		t.Fatalf("want «Частичный сбой» with one monitor down: %s", body)
	}
	for _, leak := range []string{"10.0.0.5", "connection refused", "example.com", "db-primary", "local"} {
		if strings.Contains(body, leak) {
			t.Fatalf("public status page leaks %q: %s", leak, body)
		}
	}
}

func TestWebStatusPageDisabledAndUnknown404(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spoff")

	m := statusPageMonitor(t, s, proj.ID, "hidden-monitor", "https://example.com/hidden")
	spOff, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Disabled", Enabled: false,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	const missingKey = "p_0000000000000000000missing"

	if status, body := getAnon(t, s.srv, "/status/"+spOff.PublicID); status != http.StatusNotFound {
		t.Fatalf("GET disabled page = %d, want 404: %s", status, body)
	}
	if status, body := getAnon(t, s.srv, "/status/"+missingKey); status != http.StatusNotFound {
		t.Fatalf("GET unknown key = %d, want 404: %s", status, body)
	}

	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO status_pages (project_id, public_id, title, enabled)
		VALUES ($1, $2, $3, true)`, proj.ID, missingKey, "Now Exists"); err != nil {
		t.Fatalf("insert status page: %v", err)
	}
	status, body := getAnon(t, s.srv, "/status/"+missingKey)
	if status != http.StatusOK {
		t.Fatalf("GET after create = %d, want 200 (404 must not be cached): %s", status, body)
	}
	if !strings.Contains(body, "Now Exists") {
		t.Fatalf("want fresh page content: %s", body)
	}
}

func TestWebStatusPageCached(t *testing.T) {
	s := newStatusPageStack(t)
	proj, _, _ := statusPageProject(t, s, "spcache")

	m := statusPageMonitor(t, s, proj.ID, "cached-monitor", "https://example.com/cached")
	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Cached", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Old Name", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	status, first := getAnon(t, s.srv, "/status/"+sp.PublicID)
	if status != http.StatusOK || !strings.Contains(first, "Old Name") {
		t.Fatalf("first GET = %d, want 200 with «Old Name»: %s", status, first)
	}

	sp.Title = "Renamed"
	if err := s.uptime.UpdateStatusPage(context.Background(), sp,
		[]uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "New Name", Position: 0}}); err != nil {
		t.Fatalf("update status page: %v", err)
	}

	status, second := getAnon(t, s.srv, "/status/"+sp.PublicID)
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

const statusPageStampedeRequests = 24

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

	publicID := make(map[string]string, 2)
	for _, label := range []string{"spflight-warm", "spflight-cold"} {
		sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
			ProjectID: proj.ID, Title: "Flight " + label, Enabled: true,
		}, monitors)
		if err != nil {
			t.Fatalf("create status page %s: %v", label, err)
		}
		publicID[label] = sp.PublicID
	}

	before := s.pool.Stat().AcquireCount()
	if status, body := getAnon(t, s.srv, "/status/"+publicID["spflight-warm"]); status != http.StatusOK {
		t.Fatalf("warm-up GET = %d, want 200: %s", status, body)
	}
	oneBuild := s.pool.Stat().AcquireCount() - before
	if oneBuild == 0 {
		t.Fatalf("single build made no PG queries — the counter is not measuring builds")
	}

	before = s.pool.Stat().AcquireCount()

	coldURL := s.srv.URL + "/status/" + publicID["spflight-cold"]
	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make([]int, statusPageStampedeRequests)
	bodies := make([]string, statusPageStampedeRequests)
	for i := range statusPageStampedeRequests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Get(coldURL)
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

	for i := range statusPageStampedeRequests {
		if statuses[i] != http.StatusOK {
			t.Fatalf("concurrent GET #%d = %d, want 200: %s", i, statuses[i], bodies[i])
		}
		if !strings.Contains(bodies[i], "Flight spflight-cold") || !strings.Contains(bodies[i], "Service 0") {
			t.Fatalf("concurrent GET #%d returned an incomplete page: %s", i, bodies[i])
		}
	}

	// запас 2× покрывает шум пула, не 24 независимые сборки.
	spent := s.pool.Stat().AcquireCount() - before
	if spent > 2*oneBuild {
		t.Fatalf("%d concurrent requests spent %d PG acquires (one build = %d): the cache is not single-flight",
			statusPageStampedeRequests, spent, oneBuild)
	}
}

func TestWebStatusPagesForeignMonitorRejected(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, _ := statusPageProject(t, s, "spforeign")
	otherProj, _, _ := statusPageProject(t, s, "spvictim")

	mine := statusPageMonitor(t, s, proj.ID, "own-monitor", "https://example.com/own")
	foreign := statusPageMonitor(t, s, otherProj.ID, "victim-monitor", "https://example.com/victim")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/statuspages"
	form := url.Values{
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

	status, pub := getAnon(t, s.srv, "/status/"+pages[0].PublicID)
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

func TestWebStatusPagesForeignOrigin(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, _ := statusPageProject(t, s, "sporigin")
	m := statusPageMonitor(t, s, proj.ID, "origin-monitor", "https://example.com/origin")

	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Origin", Enabled: true,
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
		{path, url.Values{"title": {"New"}, "enabled": {"on"}}},
		{spPath, url.Values{"title": {"Hacked"}, "enabled": {"on"}}},
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

func TestWebStatusPagesSettingsCRUD(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, _ := statusPageProject(t, s, "spcrud")
	m := statusPageMonitor(t, s, proj.ID, "crud-monitor", "https://example.com/crud")

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/statuspages"

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
		"title":       {"CRUD Status"},
		"description": {"desc"},
		"enabled":     {"on"},
		"monitors":    {strconv.FormatInt(m.ID, 10)},
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
	if len(pages) != 1 || pages[0].Title != "CRUD Status" || pages[0].PublicID == "" || !pages[0].Enabled {
		t.Fatalf("pages = %+v, want single enabled CRUD Status with a public_id", pages)
	}
	pageID := pages[0].ID

	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), s.srv.URL+"/status/"+pages[0].PublicID) {
		t.Fatalf("settings page must show the public URL: %s", body)
	}
	if !strings.Contains(string(body), "Public API") {
		t.Fatalf("edit form must prefill display_name: %s", body)
	}

	invalid := url.Values{
		"title":       {""},
		"description": {"Another Status"},
		"enabled":     {"on"},
		"monitors":    {strconv.FormatInt(m.ID, 10)},
	}
	resp = postForm(t, s.srv, path, invalid, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (empty title) = %d, want 422: %s", path, resp.StatusCode, body)
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

	updatePath := "/statuspages/" + strconv.FormatInt(pageID, 10)
	update := url.Values{
		"title":    {"CRUD Status"},
		"enabled":  {"on"},
		"monitors": {strconv.FormatInt(m.ID, 10)},
		"display_name_" + strconv.FormatInt(m.ID, 10): {"Renamed API"},
	}
	resp = postForm(t, s.srv, updatePath, update, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s = %d, want 303: %s", updatePath, resp.StatusCode, body)
	}
	status, pub := getAnon(t, s.srv, "/status/"+pages[0].PublicID)
	if status != http.StatusOK || !strings.Contains(pub, "Renamed API") {
		t.Fatalf("public page after update = %d: %s", status, pub)
	}

	resp = postForm(t, s.srv, updatePath+"/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
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

func TestWebStatusPagesSettingsStrangerBoundary(t *testing.T) {
	s := newStatusPageStack(t)
	proj, ownerCookie, _ := statusPageProject(t, s, "spforbid")
	_, otherOwnerCookie, _ := statusPageProject(t, s, "spother")
	_, strangerCookie := orgSettingsRegister(t, s.auth, "spforbid-stranger@example.com")

	m := statusPageMonitor(t, s, proj.ID, "forbid-monitor", "https://example.com/forbid")
	sp, err := s.uptime.CreateStatusPage(context.Background(), uptime.StatusPage{
		ProjectID: proj.ID, Title: "Forbid", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: m.ID, DisplayName: "Service", Position: 0}})
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/statuspages"

	// чужак получает 404, не 403 — существование проекта не раскрывается.
	resp := getWithCookie(t, s.srv, path, strangerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (stranger) = %d, want 404", path, resp.StatusCode)
	}

	resp = postForm(t, s.srv, path, url.Values{"title": {"x"}}, s.srv.URL, strangerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (stranger) = %d, want 404", path, resp.StatusCode)
	}

	updatePath := "/statuspages/" + strconv.FormatInt(sp.ID, 10)
	resp = postForm(t, s.srv, updatePath, url.Values{"title": {"Hacked"}}, s.srv.URL, strangerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (stranger) = %d, want 404", updatePath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, updatePath+"/delete", url.Values{}, s.srv.URL, otherOwnerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s/delete (foreign owner) = %d, want 404", updatePath, resp.StatusCode)
	}

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
		t.Fatalf("len(pages) = %d, want 1 (stranger/foreign writes must not persist)", len(pages))
	}
}
