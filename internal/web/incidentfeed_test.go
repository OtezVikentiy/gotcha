package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// incidentFeedStack — Handler + Store поверх мигрированной PG, без
// ClickHouse (лента D3 его не читает) — облегчённая версия newHostsStack
// (hosts_web_test.go) для incident-feed.
type incidentFeedStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	h      *web.Handler
	org    *org.Service
	auth   *auth.Service
	groups *incidentgroup.Store
}

// newIncidentFeedStack — wire=false воспроизводит стенд без подсистемы
// корреляции (h.IncidentGroups остаётся nil), как newHostsStack(t, false)
// для Metrics/Hosts.
func newIncidentFeedStack(t *testing.T, wire bool) *incidentFeedStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	groups := incidentgroup.NewStore(pool)
	if wire {
		h.IncidentGroups = groups
	}
	h.Register(mux)
	return &incidentFeedStack{pool: pool, srv: srv, h: h, org: orgSvc, auth: authSvc, groups: groups}
}

// seedFeedHost — минимальная строка hosts (как seedHost в
// internal/incidentgroup/group_test.go, недоступном отсюда — другой пакет).
func (s *incidentFeedStack) seedFeedHost(t *testing.T, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO hosts (project_id, name, environment, role) VALUES ($1,$2,'','') RETURNING id`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed host %s: %v", name, err)
	}
	return id
}

// seedFeedHostIncident — открытый host_incidents (kind/detail минимальны).
func (s *incidentFeedStack) seedFeedHostIncident(t *testing.T, projectID, hostID int64, kind string) int64 {
	t.Helper()
	var id int64
	if err := s.pool.QueryRow(context.Background(), `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,$3,'open',0,0,'') RETURNING id`,
		projectID, hostID, kind).Scan(&id); err != nil {
		t.Fatalf("seed host incident: %v", err)
	}
	return id
}

func incidentFeedPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/incident-feed"
}

func TestIncidentFeedEmptyState(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co", "Feed Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj", "Feed Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	for _, want := range []string{
		"Лента инцидентов",
		"Открытых групп нет — связанные сбои не обнаружены.",
		"Открытых инцидентов вне групп нет.",
		"За последние сутки ничего не закрывалось.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("пустая лента не содержит %q: %s", want, text)
		}
	}
}

func TestIncidentFeedGroupWithComposition(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-owner2@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co2", "Feed Co 2", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj2", "Feed Proj 2", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	rootHostName := "root-gw"
	rootHost := s.seedFeedHost(t, project.ID, rootHostName)
	var rootInc int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',true) RETURNING id`, project.ID, rootHost).Scan(&rootInc); err != nil {
		t.Fatalf("seed root incident: %v", err)
	}
	group, err := s.groups.EnsureGroup(ctx, project.ID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	memberHost := s.seedFeedHost(t, project.ID, "member-web")
	memberInc := s.seedFeedHostIncident(t, project.ID, memberHost, "disk")
	if err := s.groups.SetGroup(ctx, "host", memberInc, group.ID); err != nil {
		t.Fatalf("SetGroup host: %v", err)
	}

	var monitorID, uptimeMemberInc int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'api-mon','http',60) RETURNING id`, project.ID).Scan(&monitorID); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO incidents (monitor_id, notified_open, suppressed_by_dep)
		VALUES ($1,true,true) RETURNING id`, monitorID).Scan(&uptimeMemberInc); err != nil {
		t.Fatalf("seed uptime incident: %v", err)
	}
	if err := s.groups.SetGroup(ctx, "uptime", uptimeMemberInc, group.ID); err != nil {
		t.Fatalf("SetGroup uptime: %v", err)
	}

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	for _, want := range []string{
		"Хост недоступен",
		rootHostName,
		"host 1 · uptime 1 · metric 0 · slo 0",
		"подавлен зависимостью",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("карточка группы не содержит %q: %s", want, text)
		}
	}
}

func TestIncidentFeedAccessDenied(t *testing.T) {
	s := newIncidentFeedStack(t, true)
	ctx := context.Background()
	ownerID, _ := orgSettingsRegister(t, s.auth, "feed-owner3@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co3", "Feed Co 3", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj3", "Feed Proj 3", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "feed-outsider@example.com")

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}
}

func TestIncidentFeedNilStore(t *testing.T) {
	s := newIncidentFeedStack(t, false)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "feed-owner4@example.com")
	o, err := s.org.CreateOrg(ctx, "feed-co4", "Feed Co 4", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "feed-proj4", "Feed Proj 4", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	resp := getWithCookie(t, s.srv, incidentFeedPath(project.ID), ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("h.IncidentGroups == nil: status = %d, want 404", resp.StatusCode)
	}
}
