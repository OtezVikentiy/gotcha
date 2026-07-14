package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
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

// monitorsStack — own stand (like uptimeStack in heartbeat_test.go), but
// also wires h.UptimeQuery (ClickHouse reads) since the monitor list/detail
// pages need uptime%/latency/recent-checks from Query, not just
// Uptime/UptimeWriter which heartbeat needs.
type monitorsStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	uptime *uptime.Service
	writer *uptime.ResultWriter
}

func newMonitorsStack(t *testing.T) *monitorsStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query // страницы мониторов его не используют

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

	return &monitorsStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, uptime: uptimeSvc, writer: writer}
}

// flush drains the ResultWriter's buffer into ClickHouse synchronously (same
// pattern as query_test.go): further Add calls after this would sit unread
// until t.Cleanup's second Close, so tests call this once after seeding all
// the CH rows they need.
func (s *monitorsStack) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.writer.Close(ctx); err != nil {
		t.Fatalf("flush writer: %v", err)
	}
}

func monHTTPConfig(t *testing.T, target string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(uptime.HTTPConfig{Method: "GET", URL: target})
	if err != nil {
		t.Fatalf("marshal http config: %v", err)
	}
	return raw
}

func baseMonitor(projectID int64, name string) uptime.Monitor {
	return uptime.Monitor{
		ProjectID:         projectID,
		Name:              name,
		Kind:              uptime.KindHTTP,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     1,
		RecoveryThreshold: 1,
		Consensus:         uptime.ConsensusMajority,
		SSLAlertDays:      14,
	}
}

// addTeamAccess gives userID view access to projectID the same way a
// non-admin project member normally gets it (org.RoleMember alone does
// NOT grant CanAccessProject — see org.accessCondition): a team, attached
// to the project, with userID as a member.
func addTeamAccess(t *testing.T, orgSvc *org.Service, orgID, projectID, userID int64, teamSlug string) {
	t.Helper()
	team, err := orgSvc.CreateTeam(context.Background(), orgID, teamSlug, teamSlug)
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := orgSvc.AddTeamMember(context.Background(), team.ID, userID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := orgSvc.AttachTeam(context.Background(), projectID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}
}

// TestWebMonitorsList — owner sees both monitors with an uptime % and at
// least one <svg (availability bars); the monitor with zero checks shows
// "no data" instead of crashing; member (view access via team, not
// owner/admin) sees the page but not the "New monitor" link; an outsider
// gets 404.
func TestWebMonitorsList(t *testing.T) {
	s := newMonitorsStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "monlist-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "monlist-member@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "monlist-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "monlist-co", "MonList Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "monlist-proj", "MonList Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, s.org, o.ID, proj.ID, memberID, "monlist-team")

	withData := baseMonitor(proj.ID, "With data")
	withData.Config = monHTTPConfig(t, "https://example.com/health")
	createdWith, err := s.uptime.Create(context.Background(), withData, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor with data: %v", err)
	}

	noData := baseMonitor(proj.ID, "No data")
	noData.Config = monHTTPConfig(t, "https://example.com/quiet")
	if _, err := s.uptime.Create(context.Background(), noData, []string{"local"}, nil); err != nil {
		t.Fatalf("create monitor without data: %v", err)
	}

	// checkAt sits a couple of minutes in the past, not literally "now" —
	// see the longer comment in TestWebMonitorDetail on why a comfortable
	// margin from the query's own time.Now() avoids a ClickHouse
	// driver precision quirk on bare time.Time query parameters.
	checkAt := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := s.uptime.ApplyResult(context.Background(), createdWith.ID, "local", true, "", checkAt); err != nil {
		t.Fatalf("apply result: %v", err)
	}
	s.writer.Add(proj.ID, createdWith.ID, "local", checkAt, uptime.Result{OK: true, StatusCode: 200, TotalMs: 120})
	s.flush(t)

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors"

	// Owner GET -> 200: both monitor names, an <svg availability bar, the
	// "no data" monitor, a computed uptime % for the one with data, and the
	// "New monitor" link (owner/admin only).
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	for _, want := range []string{"With data", "No data", "<svg", "no data", "New monitor", "100.00%"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (owner) missing %q: %s", path, want, body)
		}
	}

	// Member (view access, not owner/admin) GET -> 200, but no "New monitor"
	// link.
	resp = getWithCookie(t, s.srv, path, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (member) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "New monitor") {
		t.Fatalf("GET %s (member) must not show New monitor link: %s", path, body)
	}

	// Outsider (no org membership at all) -> 404.
	resp = getWithCookie(t, s.srv, path, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", path, resp.StatusCode)
	}
}

// TestWebMonitorDetail — the detail page shows the monitor's incidents and
// recent checks (with a real error/status code), SSL expiry, and hides the
// Pause/Edit/Delete controls from a member without owner/admin role; an
// outsider gets 404.
func TestWebMonitorDetail(t *testing.T) {
	s := newMonitorsStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "mondetail-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "mondetail-member@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "mondetail-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "mondetail-co", "MonDetail Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "mondetail-proj", "MonDetail Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, s.org, o.ID, proj.ID, memberID, "mondetail-team")

	m := baseMonitor(proj.ID, "API health")
	m.Config = monHTTPConfig(t, "https://example.com/health")
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	// checkAt/now: check timestamps sit a couple of minutes in the past
	// (not literally "now") on purpose — the ClickHouse driver binds a bare
	// time.Time query parameter at whole-second precision (toDateTime, not
	// toDateTime64), so a check inserted and then queried within the same
	// wall-clock second as the handler's own time.Now() could be excluded
	// by the "timestamp < to" boundary purely due to that precision loss.
	// A comfortable multi-minute margin — realistic anyway, since a check
	// always happened measurably before any page load — avoids the flake
	// without touching internal/uptime/query.go.
	now := time.Now().UTC()
	s.writer.Add(proj.ID, created.ID, "local", now.Add(-2*time.Minute), uptime.Result{
		OK: true, StatusCode: 200, TotalMs: 80, DNSMs: 5, ConnectMs: 10, TLSMs: 15, TTFBMs: 30,
	})
	s.writer.Add(proj.ID, created.ID, "local", now.Add(-time.Minute), uptime.Result{
		OK: false, StatusCode: 500, Error: "boom", TotalMs: 200,
	})
	s.flush(t)

	if _, _, err := s.uptime.OpenIncident(context.Background(), created.ID, "boom", []string{"local"}, false); err != nil {
		t.Fatalf("open incident: %v", err)
	}
	if _, _, err := s.uptime.ResolveIncident(context.Background(), created.ID, now); err != nil {
		t.Fatalf("resolve incident: %v", err)
	}

	expires := now.Add(20 * 24 * time.Hour)
	if err := s.uptime.SetSSLExpiry(context.Background(), created.ID, expires); err != nil {
		t.Fatalf("set ssl expiry: %v", err)
	}

	path := "/monitors/" + strconv.FormatInt(created.ID, 10)

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	for _, want := range []string{"API health", "boom", "500", "<svg", "Pause", "Delete", "Edit", "d left"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (owner) missing %q: %s", path, want, body)
		}
	}

	// Member (view access, not owner/admin) -> 200, no management buttons.
	resp = getWithCookie(t, s.srv, path, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (member) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	for _, unwanted := range []string{"Pause", "Delete", "Edit"} {
		if strings.Contains(string(body), unwanted) {
			t.Fatalf("GET %s (member) must not show %q: %s", path, unwanted, body)
		}
	}

	// Outsider -> 404.
	resp = getWithCookie(t, s.srv, path, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", path, resp.StatusCode)
	}
}

// TestWebMonitorDetailNoChecksShowsNoDataAndDoesNotCrash — a freshly created
// monitor without a single check must still render 200 with "no data"
// instead of failing (division by zero in uptime %, empty chart, etc.).
func TestWebMonitorDetailNoChecksShowsNoDataAndDoesNotCrash(t *testing.T) {
	s := newMonitorsStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "monempty-owner@example.com")

	o, err := s.org.CreateOrg(context.Background(), "monempty-co", "MonEmpty Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "monempty-proj", "MonEmpty Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	m := baseMonitor(proj.ID, "Quiet monitor")
	m.Config = monHTTPConfig(t, "https://example.com/quiet")
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	path := "/monitors/" + strconv.FormatInt(created.ID, 10)
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "no data") {
		t.Fatalf("GET %s missing 'no data': %s", path, body)
	}
	if !strings.Contains(string(body), "Нет проверок") {
		t.Fatalf("GET %s missing empty-checks message: %s", path, body)
	}
	if !strings.Contains(string(body), "Нет инцидентов") {
		t.Fatalf("GET %s missing empty-incidents message: %s", path, body)
	}
}

// TestWebMonitorPauseResumeDelete — sameOrigin + requireProjectRole gate all
// three POST mutations: no Origin -> 403, member (not owner/admin) -> 404,
// owner can pause (enabled=false persisted), resume (enabled=true again),
// and delete (monitor gone).
func TestWebMonitorPauseResumeDelete(t *testing.T) {
	s := newMonitorsStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "monpause-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "monpause-member@example.com")

	o, err := s.org.CreateOrg(context.Background(), "monpause-co", "MonPause Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "monpause-proj", "MonPause Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	m := baseMonitor(proj.ID, "Pausable")
	m.Config = monHTTPConfig(t, "https://example.com/health")
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	pausePath := "/monitors/" + strconv.FormatInt(created.ID, 10) + "/pause"
	resumePath := "/monitors/" + strconv.FormatInt(created.ID, 10) + "/resume"
	deletePath := "/monitors/" + strconv.FormatInt(created.ID, 10) + "/delete"

	// No Origin -> 403.
	resp := postForm(t, s.srv, pausePath, url.Values{}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", pausePath, resp.StatusCode)
	}

	// Member -> 404.
	resp = postForm(t, s.srv, pausePath, url.Values{}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", pausePath, resp.StatusCode)
	}

	// Owner pauses -> 303, enabled=false persisted.
	resp = postForm(t, s.srv, pausePath, url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner) status = %d, want 303", pausePath, resp.StatusCode)
	}
	got, err := s.uptime.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get after pause: %v", err)
	}
	if got.Enabled {
		t.Fatalf("Enabled = true after pause, want false")
	}

	// Owner resumes -> 303, enabled=true again.
	resp = postForm(t, s.srv, resumePath, url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner) status = %d, want 303", resumePath, resp.StatusCode)
	}
	got, err = s.uptime.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get after resume: %v", err)
	}
	if !got.Enabled {
		t.Fatalf("Enabled = false after resume, want true")
	}

	// Member delete -> 404, monitor still there.
	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", deletePath, resp.StatusCode)
	}

	// Owner deletes -> 303, monitor gone.
	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner) status = %d, want 303", deletePath, resp.StatusCode)
	}
	if _, err := s.uptime.Get(context.Background(), created.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("Get after delete: err = %v, want ErrNotFound", err)
	}
}

// TestWebIssuesPageHasMonitorsLink — the project's issues page links to its
// monitors list (spec: navigation), the same "dead link fixed ahead of time"
// convention as the existing Project settings/Alerts links.
func TestWebIssuesPageHasMonitorsLink(t *testing.T) {
	s := newMonitorsStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "monlink-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "monlink-co", "MonLink Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "monlink-proj", "MonLink Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/issues"
	monitorsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors"

	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), monitorsPath) {
		t.Fatalf("GET %s missing monitors link %q: %s", issuesPath, monitorsPath, body)
	}
}
