package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type sqlCountingTracer struct {
	mu     sync.Mutex
	counts map[string]int
}

func newSQLCountingTracer(needles ...string) *sqlCountingTracer {
	c := &sqlCountingTracer{counts: make(map[string]int, len(needles))}
	for _, n := range needles {
		c.counts[n] = 0
	}
	return c
}

func (c *sqlCountingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	for needle := range c.counts {
		if strings.Contains(data.SQL, needle) {
			c.counts[needle]++
		}
	}
	return ctx
}

func (c *sqlCountingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *sqlCountingTracer) count(needle string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[needle]
}

func (c *sqlCountingTracer) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for needle := range c.counts {
		c.counts[needle] = 0
	}
}

// testenv.MigratedPG трейсер принять не может — ему нужен голый DSN, эта обёртка даёт его отдельно.
func tracedPG(t *testing.T, tracer *sqlCountingTracer) *pgxpool.Pool {
	t.Helper()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse pg config: %v", err)
	}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTracedServer(t *testing.T, h *web.Handler, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Предикат ANY($1) отличает пакетный запрос от построчного (тот дал бы = $1) —
// так тест ловит N+1 регресс.
const (
	sqlTeamMembers      = "WHERE tm.team_id = ANY($1)"
	sqlTeamProjects     = "WHERE pt.team_id = ANY($1)"
	sqlStatusPageMons   = "WHERE status_page_id = ANY($1)"
	sqlMaintWindows     = "FROM maintenance_windows"
	sqlLeasedJobsLookup = "FROM check_queue q\n\t\tJOIN monitors m"
	sqlClaimJobs        = "DELETE FROM check_queue"
)

func TestWebTeamsQueriesDoNotGrowWithTeams(t *testing.T) {
	tracer := newSQLCountingTracer(sqlTeamMembers, sqlTeamProjects)
	pool := tracedPG(t, tracer)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	mux := http.NewServeMux()
	h := web.New(authSvc, orgSvc, nil, nil, "")
	srv := newTracedServer(t, h, mux)
	h.BaseURL = srv.URL
	h.Register(mux)

	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "teams-qc@example.com")
	o, err := orgSvc.CreateOrg(ctx, "teams-qc", "Teams QC", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	teamsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/teams"

	addTeam := func(i int) {
		t.Helper()
		slug := "team-" + strconv.Itoa(i)
		team, err := orgSvc.CreateTeam(ctx, o.ID, slug, slug)
		if err != nil {
			t.Fatalf("create team: %v", err)
		}
		if err := orgSvc.AddTeamMember(ctx, team.ID, ownerID); err != nil {
			t.Fatalf("add team member: %v", err)
		}
		project, err := orgSvc.CreateProject(ctx, o.ID, "proj-"+strconv.Itoa(i), "proj-"+strconv.Itoa(i), "go")
		if err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := orgSvc.AttachTeam(ctx, project.ID, team.ID); err != nil {
			t.Fatalf("attach team: %v", err)
		}
	}

	addTeam(1)
	tracer.reset()
	status, body := getBody(t, srv, teamsPath, ownerCookie)
	if status != http.StatusOK {
		t.Fatalf("GET %s (1 team) status = %d, want 200", teamsPath, status)
	}
	if !strings.Contains(body, "team-1") || !strings.Contains(body, "proj-1") {
		t.Fatalf("GET %s: team or its project missing: %s", teamsPath, body)
	}
	membersOne, projectsOne := tracer.count(sqlTeamMembers), tracer.count(sqlTeamProjects)

	addTeam(2)
	addTeam(3)
	tracer.reset()
	status, body = getBody(t, srv, teamsPath, ownerCookie)
	if status != http.StatusOK {
		t.Fatalf("GET %s (3 teams) status = %d, want 200", teamsPath, status)
	}
	for _, want := range []string{"team-1", "team-2", "team-3", "proj-1", "proj-2", "proj-3", "teams-qc@example.com"} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET %s (3 teams): %q missing — batching lost a row", teamsPath, want)
		}
	}
	if got := tracer.count(sqlTeamMembers); got != membersOne || got != 1 {
		t.Fatalf("team members queries: 1 team -> %d, 3 teams -> %d, want exactly 1 both times", membersOne, got)
	}
	if got := tracer.count(sqlTeamProjects); got != projectsOne || got != 1 {
		t.Fatalf("team projects queries: 1 team -> %d, 3 teams -> %d, want exactly 1 both times", projectsOne, got)
	}
}

func TestWebStatusPagesSettingsQueriesDoNotGrowWithPages(t *testing.T) {
	tracer := newSQLCountingTracer(sqlStatusPageMons)
	pool := tracedPG(t, tracer)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	uptimeSvc := uptime.NewService(pool)
	mux := http.NewServeMux()
	h := web.New(authSvc, orgSvc, nil, nil, "")
	srv := newTracedServer(t, h, mux)
	h.BaseURL = srv.URL
	h.Uptime = uptimeSvc
	h.Register(mux)

	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "sp-qc@example.com")
	o, err := orgSvc.CreateOrg(ctx, "sp-qc", "SP QC", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := orgSvc.CreateProject(ctx, o.ID, "sp-qc-proj", "P", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	m := baseMonitor(project.ID, "mon-a")
	m.Config = monHTTPConfig(t, "https://example.com/health")
	mon, err := uptimeSvc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/statuspages"

	addPage := func(i int) {
		t.Helper()
		_, err := uptimeSvc.CreateStatusPage(ctx, uptime.StatusPage{
			ProjectID: project.ID, Title: "Page " + strconv.Itoa(i), Enabled: true,
		}, []uptime.StatusPageMonitor{{MonitorID: mon.ID, DisplayName: "Mon " + strconv.Itoa(i), Position: 0}})
		if err != nil {
			t.Fatalf("create status page: %v", err)
		}
	}

	addPage(1)
	tracer.reset()
	status, _ := getBody(t, srv, path, ownerCookie)
	if status != http.StatusOK {
		t.Fatalf("GET %s (1 page) status = %d, want 200", path, status)
	}
	one := tracer.count(sqlStatusPageMons)

	addPage(2)
	addPage(3)
	tracer.reset()
	status, body := getBody(t, srv, path, ownerCookie)
	if status != http.StatusOK {
		t.Fatalf("GET %s (3 pages) status = %d, want 200", path, status)
	}
	for _, want := range []string{"Page 1", "Page 2", "Page 3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET %s (3 pages): %q missing", path, want)
		}
	}
	if got := strings.Count(body, `value="`+strconv.FormatInt(mon.ID, 10)+`" aria-label="mon-a" checked`); got < 3 {
		t.Fatalf("GET %s (3 pages): checked monitor appears %d times, want at least 3 — per-page selection lost", path, got)
	}
	if got := tracer.count(sqlStatusPageMons); got != one || got != 1 {
		t.Fatalf("status page monitors queries: 1 page -> %d, 3 pages -> %d, want exactly 1 both times", one, got)
	}
}

// ClickHouse настоящий, с результатами проверок — без корзин провайдер за окнами
// не ходил бы вовсе, и тест ничего не доказывал.
func TestWebSLOsQueriesDoNotGrowWithSLOs(t *testing.T) {
	tracer := newSQLCountingTracer(sqlMaintWindows)
	pool := tracedPG(t, tracer)
	ch := testenv.MigratedCH(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	uptimeSvc := uptime.NewService(pool)
	store := slo.NewStore(pool)

	writer := uptime.NewResultWriter(ch)
	go writer.Run()

	mux := http.NewServeMux()
	h := web.New(authSvc, orgSvc, nil, nil, "")
	srv := newTracedServer(t, h, mux)
	h.BaseURL = srv.URL
	h.Uptime = uptimeSvc
	h.SLO = store
	h.SLOProviders = slo.Providers(nil, uptime.NewQuery(ch), uptimeSvc, 0)
	h.Register(mux)

	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "slo-qc@example.com")
	o, err := orgSvc.CreateOrg(ctx, "slo-qc", "SLO QC", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := orgSvc.CreateProject(ctx, o.ID, "slo-qc-proj", "P", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	m := baseMonitor(project.ID, "mon-slo")
	m.Config = monHTTPConfig(t, "https://example.com/health")
	mon, err := uptimeSvc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if _, err := uptimeSvc.CreateWindow(ctx, uptime.Window{
		ProjectID: project.ID, Name: "nightly", Weekly: true, Weekday: int(time.Now().UTC().Weekday()),
		StartTime: "03:00", EndTime: "04:00", Timezone: "UTC",
	}); err != nil {
		t.Fatalf("create window: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < 6; i++ {
		writer.Add(project.ID, mon.ID, "local", now.Add(-time.Duration(i+1)*time.Hour), uptime.Result{OK: true, StatusCode: 200})
	}
	flushCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := writer.Close(flushCtx); err != nil {
		t.Fatalf("flush results: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/slos"

	addSLO := func(i int) {
		t.Helper()
		monID := mon.ID
		if _, err := store.Create(ctx, slo.SLO{
			ProjectID: project.ID, Name: "uptime-" + strconv.Itoa(i), Kind: slo.SLIUptime,
			Target: 0.99, WindowDays: 7, MonitorID: &monID,
		}); err != nil {
			t.Fatalf("create slo: %v", err)
		}
	}

	addSLO(1)
	tracer.reset()
	status, body := getBody(t, srv, path, ownerCookie)
	if status != http.StatusOK {
		t.Fatalf("GET %s (1 slo) status = %d, want 200", path, status)
	}
	if !strings.Contains(body, "uptime-1") || !strings.Contains(body, "100.0%") {
		t.Fatalf("GET %s (1 slo): attainment not rendered — no buckets, the windows path is not exercised: %s", path, body)
	}
	one := tracer.count(sqlMaintWindows)
	if one != 1 {
		t.Fatalf("maintenance windows queries with 1 SLO = %d, want exactly 1", one)
	}

	addSLO(2)
	addSLO(3)
	tracer.reset()
	status, body = getBody(t, srv, path, ownerCookie)
	if status != http.StatusOK {
		t.Fatalf("GET %s (3 slos) status = %d, want 200", path, status)
	}
	for _, want := range []string{"uptime-1", "uptime-2", "uptime-3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET %s (3 slos): %q missing", path, want)
		}
	}
	if got := tracer.count(sqlMaintWindows); got != 1 {
		t.Fatalf("maintenance windows queries with 3 SLOs = %d, want exactly 1 (one per page, not per row)", got)
	}
}

// Применение результатов к состоянию монитора остаётся построчным (машина
// состояний) — здесь эти запросы не считаем.
func TestProbeResultsQueriesDoNotGrowWithResults(t *testing.T) {
	tracer := newSQLCountingTracer(sqlLeasedJobsLookup, sqlClaimJobs)
	pool := tracedPG(t, tracer)
	ch := testenv.MigratedCH(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	uptimeSvc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(cctx)
	})

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	h.Uptime = uptimeSvc
	h.UptimeWriter = writer
	h.UptimeIngestor = &uptime.Ingestor{Svc: uptimeSvc, Writer: writer}
	h.Register(mux)
	s := &probeStack{pool: pool, srv: srv, uptime: uptimeSvc, writer: writer}

	ctx := context.Background()
	orgID, pid := newOrgProject(t, pool)
	_, token, err := uptimeSvc.CreateProbe(ctx, orgID, "eu-west", "Probe QC")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}
	const n = 4
	for i := 0; i < n; i++ {
		if _, err := uptimeSvc.Create(ctx, probeHTTPMonitor(t, pid), []string{"eu-west"}, nil); err != nil {
			t.Fatalf("Create monitor %d: %v", i, err)
		}
	}
	if _, err := uptimeSvc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	resp := probePost(t, s, "/probe/lease", token, uptime.LeaseRequest{Limit: n})
	var lease uptime.LeaseResponse
	decodeJSON(t, resp, &lease)
	if len(lease.Jobs) != n {
		t.Fatalf("lease.Jobs = %d, want %d", len(lease.Jobs), n)
	}

	req := uptime.ResultsRequest{}
	for _, j := range lease.Jobs {
		req.Results = append(req.Results, uptime.ResultDTO{QueueID: j.QueueID, OK: true, StatusCode: 200})
	}
	req.Results = append(req.Results, uptime.ResultDTO{QueueID: 999_999, OK: true, StatusCode: 200})

	tracer.reset()
	rresp := probePost(t, s, "/probe/results", token, req)
	if rresp.StatusCode != http.StatusOK {
		drain(rresp)
		t.Fatalf("status = %d, want 200", rresp.StatusCode)
	}
	var out uptime.ResultsResponse
	decodeJSON(t, rresp, &out)
	if out.Accepted != n || out.Rejected != 1 {
		t.Fatalf("results = %+v, want accepted %d rejected 1", out, n)
	}
	if got := tracer.count(sqlLeasedJobsLookup); got != 1 {
		t.Fatalf("leased job lookups = %d for %d results, want exactly 1", got, n+1)
	}
	if got := tracer.count(sqlClaimJobs); got != 1 {
		t.Fatalf("claim statements = %d for %d results, want exactly 1", got, n+1)
	}
	pending, err := uptimeSvc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingCount() = %d, want 0 (all leased jobs must be claimed)", pending)
	}

	rresp = probePost(t, s, "/probe/results", token, req)
	decodeJSON(t, rresp, &out)
	if out.Accepted != 0 || out.Rejected != n+1 {
		t.Fatalf("resend results = %+v, want all %d rejected (jobs no longer leased)", out, n+1)
	}
}

func TestProbeResultsDuplicateQueueIDAppliedOnce(t *testing.T) {
	s := newProbeStack(t)
	ctx := context.Background()
	orgID, pid := newOrgProject(t, s.pool)
	_, token, err := s.uptime.CreateProbe(ctx, orgID, "eu-west", "Probe Dup")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}
	created, err := s.uptime.Create(ctx, probeHTTPMonitor(t, pid), []string{"eu-west"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.uptime.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	resp := probePost(t, s, "/probe/lease", token, nil)
	var lease uptime.LeaseResponse
	decodeJSON(t, resp, &lease)
	if len(lease.Jobs) != 1 {
		t.Fatalf("lease.Jobs = %d, want 1", len(lease.Jobs))
	}
	dto := uptime.ResultDTO{QueueID: lease.Jobs[0].QueueID, OK: false, StatusCode: 500, Error: "boom"}
	rresp := probePost(t, s, "/probe/results", token, uptime.ResultsRequest{Results: []uptime.ResultDTO{dto, dto}})
	if rresp.StatusCode != http.StatusOK {
		drain(rresp)
		t.Fatalf("status = %d, want 200", rresp.StatusCode)
	}
	var out uptime.ResultsResponse
	decodeJSON(t, rresp, &out)
	if out.Accepted != 1 || out.Rejected != 0 || out.Dropped != 1 {
		t.Fatalf("results = %+v, want accepted 1 rejected 0 dropped 1 (second copy loses the claim race)", out)
	}
	if got := len(s.results); got != 1 {
		t.Fatalf("OnResult calls = %d, want exactly 1 — duplicate queue_id applied twice", got)
	}
	states, err := s.uptime.States(ctx, created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].ConsecutiveFails != 1 {
		t.Fatalf("states = %+v, want one eu-west state with consecutive_fails 1 (single application)", states)
	}
	pending, err := s.uptime.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingCount() = %d, want 0", pending)
	}
}
