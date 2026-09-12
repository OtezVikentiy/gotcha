package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type confirmLimitStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	h    *web.Handler

	adminUID    int64
	adminCookie *http.Cookie

	otherUID    int64
	otherCookie *http.Cookie

	orgID     int64
	projectID int64
	teamID    int64

	httpMonitorID      int64
	heartbeatMonitorID int64
	exportJobID        int64
	statusPageID       int64
}

func newConfirmLimitStack(t *testing.T) *confirmLimitStack {
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
	h.Exports = export.NewStore(pool)
	h.ExportDir = t.TempDir()
	h.AlertDeps = depsuppress.NewStore(pool)
	h.Register(mux)

	ctx := context.Background()
	adminUID, err := authSvc.Register(ctx, "confirm-limit-admin@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	adminToken, err := authSvc.CreateSession(ctx, adminUID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	otherUID, err := authSvc.Register(ctx, "confirm-limit-other@example.com", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register other: %v", err)
	}
	otherToken, err := authSvc.CreateSession(ctx, otherUID)
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}

	o, err := orgSvc.CreateOrg(ctx, "confirm-limit-org", "Confirm Limit", adminUID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(ctx, o.ID, "confirm-limit-proj", "Confirm Limit Proj", "other")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	team, err := orgSvc.CreateTeam(ctx, o.ID, "confirm-limit-team", "Confirm Limit Team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	httpM := baseMonitor(proj.ID, "confirm-limit http")
	httpM.Config = monHTTPConfig(t, "https://example.invalid/health")
	createdHTTP, err := uptimeSvc.Create(ctx, httpM, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create http monitor: %v", err)
	}

	hbM := uptime.Monitor{
		ProjectID:          proj.ID,
		Name:               "confirm-limit heartbeat",
		Kind:               uptime.KindHeartbeat,
		Enabled:            true,
		IntervalSeconds:    60,
		TimeoutSeconds:     10,
		FailThreshold:      3,
		RecoveryThreshold:  1,
		Consensus:          uptime.ConsensusMajority,
		RemindEveryMinutes: 0,
		SSLAlertDays:       14,
		Config:             heartbeatConfigJSON(t, uptime.HeartbeatConfig{GraceSeconds: 60}),
	}
	createdHB, err := uptimeSvc.Create(ctx, hbM, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create heartbeat monitor: %v", err)
	}

	jobID, err := h.Exports.Enqueue(ctx, export.Job{
		ProjectID: proj.ID,
		CreatedBy: adminUID,
		Kind:      export.KindIssues,
		Format:    export.FormatCSV,
		Params:    export.Params{Since: time.Now().Add(-24 * time.Hour), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("enqueue export job: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE export_jobs SET status='done', finished_at=now(), expires_at=now()+interval '7 days' WHERE id=$1`,
		jobID); err != nil {
		t.Fatalf("mark export job done: %v", err)
	}

	sp, err := uptimeSvc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: proj.ID,
		Title:     "Confirm Limit Status",
		Enabled:   false,
	}, nil)
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	return &confirmLimitStack{
		pool: pool, srv: srv, h: h,
		adminUID:    adminUID,
		adminCookie: &http.Cookie{Name: auth.CookieName, Value: adminToken},
		otherUID:    otherUID,
		otherCookie: &http.Cookie{Name: auth.CookieName, Value: otherToken},
		orgID:       o.ID, projectID: proj.ID, teamID: team.ID,
		httpMonitorID: createdHTTP.ID, heartbeatMonitorID: createdHB.ID,
		exportJobID: jobID, statusPageID: sp.ID,
	}
}

func TestConfirmAndSwitchHandlersOversizedBodyReturns413(t *testing.T) {
	s := newConfirmLimitStack(t)
	huge := strings.Repeat("x", 70_000)

	cases := []struct {
		name   string
		path   string
		cookie *http.Cookie
	}{
		{"monitorDelete", fmt.Sprintf("/monitors/%d/delete", s.httpMonitorID), s.adminCookie},
		{"monitorHeartbeatRegenerate", fmt.Sprintf("/monitors/%d/heartbeat/regenerate", s.heartbeatMonitorID), s.adminCookie},
		{"exportsDelete", fmt.Sprintf("/projects/%d/exports/%d/delete", s.projectID, s.exportJobID), s.adminCookie},
		{"projectSettingsDelete", fmt.Sprintf("/projects/%d/settings/delete", s.projectID), s.adminCookie},
		{"teamDelete", fmt.Sprintf("/teams/%d/delete", s.teamID), s.adminCookie},
		{"profileDelete", "/profile/delete", s.otherCookie},
		{"profileInstanceAdminTransfer", "/profile/instance-admin/transfer", s.adminCookie},
		{"orgSettingsDelete", fmt.Sprintf("/orgs/%d/settings/delete", s.orgID), s.adminCookie},
		{"orgSettingsLeave", fmt.Sprintf("/orgs/%d/settings/leave", s.orgID), s.adminCookie},
		{"orgSettingsSSODelete", fmt.Sprintf("/orgs/%d/settings/sso/delete", s.orgID), s.adminCookie},
		{"statusPagesDelete", fmt.Sprintf("/statuspages/%d/delete", s.statusPageID), s.adminCookie},
		{"alertSuppressionDelete", fmt.Sprintf("/projects/%d/alert-suppression/1/delete", s.projectID), s.adminCookie},
		{"themeSwitch", "/settings/theme", nil},
		{"localeSwitch", "/settings/locale", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postForm(t, s.srv, tc.path, url.Values{"pad": {huge}, "confirmed": {"yes"}}, s.srv.URL, tc.cookie)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s: status = %d, want 413: %s", tc.path, resp.StatusCode, body)
			}
		})
	}
}
