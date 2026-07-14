package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// monitorDetailStack — reuses the same setup as monitors_test.go for
// consistency (includes ClickHouse Query).
type monitorDetailStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	uptime *uptime.Service
	writer *uptime.ResultWriter
	alerts *alert.Service
}

func newMonitorDetailStack(t *testing.T) *monitorDetailStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query

	uptimeSvc := uptime.NewService(pool)
	alertSvc := alert.NewService(pool)
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
	h.Alerts = alertSvc
	h.Register(mux)

	return &monitorDetailStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, uptime: uptimeSvc, writer: writer, alerts: alertSvc}
}

// TestWebMonitorDetailHeartbeatTokenHiddenFromMember — a member (non-admin)
// opening a heartbeat monitor's detail page must NOT see the heartbeat token
// in the response body; an owner must see it. This is a critical security test:
// the token is a bearer secret — whoever has it can fake a heartbeat and mask
// real downtime.
func TestWebMonitorDetailHeartbeatTokenHiddenFromMember(t *testing.T) {
	s := newMonitorDetailStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hb-detail-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "hb-detail-member@example.com")

	o, err := s.org.CreateOrg(context.Background(), "hb-detail-co", "HB Detail Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "hb-detail-proj", "HB Detail Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, s.org, o.ID, proj.ID, memberID, "hb-detail-team")

	// Create a heartbeat monitor.
	hbConfig := uptime.HeartbeatConfig{GraceSeconds: 120}
	hbConfigJSON, err := json.Marshal(hbConfig)
	if err != nil {
		t.Fatalf("marshal heartbeat config: %v", err)
	}

	m := uptime.Monitor{
		ProjectID:         proj.ID,
		Name:              "Heartbeat monitor",
		Kind:              uptime.KindHeartbeat,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     1,
		RecoveryThreshold: 1,
		Consensus:         uptime.ConsensusMajority,
		SSLAlertDays:      14,
		Config:            hbConfigJSON,
	}
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create heartbeat monitor: %v", err)
	}

	path := "/monitors/" + strconv.FormatInt(created.ID, 10)

	// Owner GET -> 200, must contain the heartbeat token.
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, created.HeartbeatToken) {
		t.Fatalf("GET %s (owner) missing heartbeat token %q in body: %s", path, created.HeartbeatToken, bodyStr)
	}
	if !strings.Contains(bodyStr, "Heartbeat ping") {
		t.Fatalf("GET %s (owner) missing 'Heartbeat ping' section: %s", path, bodyStr)
	}

	// Member (view access, not owner/admin) GET -> 200, must NOT contain the
	// heartbeat token or the Heartbeat ping section.
	resp = getWithCookie(t, s.srv, path, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (member) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	bodyStr = string(body)
	if strings.Contains(bodyStr, created.HeartbeatToken) {
		t.Fatalf("GET %s (member) must NOT show heartbeat token: %s", path, bodyStr)
	}
	if strings.Contains(bodyStr, "Heartbeat ping") {
		t.Fatalf("GET %s (member) must NOT show Heartbeat ping section: %s", path, bodyStr)
	}
}

// TestWebMonitorCreateInvalidTCPPortReturns422 — POST monitor create with
// tcp_port=999999 (out of range) → 422 (not 500). This is a cheap validation
// test to ensure numeric bounds are checked at the HTTP layer.
func TestWebMonitorCreateInvalidTCPPortReturns422(t *testing.T) {
	s := newMonitorDetailStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "tcpport-owner@example.com")

	o, err := s.org.CreateOrg(context.Background(), "tcpport-co", "TCPPort Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "tcpport-proj", "TCPPort Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	createPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors"
	form := url.Values{
		"name":                   {"TCP monitor with bad port"},
		"kind":                   {"tcp"},
		"tcp_host":               {"example.com"},
		"tcp_port":               {"999999"}, // Out of range (max 65535)
		"interval_seconds":       {"60"},
		"timeout_seconds":        {"10"},
		"fail_threshold":         {"1"},
		"recovery_threshold":     {"1"},
		"consensus":              {"any"},
	}

	resp := postForm(t, s.srv, createPath, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (tcp_port=999999) status = %d, want 422, body: %s", createPath, resp.StatusCode, body)
	}
}
