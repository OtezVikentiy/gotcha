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

// TestWebMonitorDetailHeartbeatSectionOperatorGated — открывая деталь
// heartbeat-монитора, НИКТО (ни owner, ни участник команды) не видит сырой
// токен на обычном GET — он хранится только хешем (sha256) и показывается
// один раз сразу после create/regenerate (см. TestWebMonitorHeartbeatCreateShowsPingURL,
// TestWebMonitorHeartbeatRegenerate). Это критичный security-тест: токен —
// bearer-секрет, у кого он есть — может подделать heartbeat и замаскировать
// реальный даунтайм. Саму секцию «Heartbeat-пинг» (с кнопкой Regenerate) с
// задачи 2 (спека 2026-08-08) видят и owner, и участник команды — карточка
// гейтится canOperate (canOperateProject), не только owner/admin.
func TestWebMonitorDetailHeartbeatSectionOperatorGated(t *testing.T) {
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

	// Owner GET -> 200. Сырой токен на чтении НЕ показывается (в БД — хеш):
	// owner видит секцию Heartbeat-пинг с кнопкой перевыпуска, но не сам токен.
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	bodyStr := string(body)
	if strings.Contains(bodyStr, created.HeartbeatToken) {
		t.Fatalf("GET %s (owner) must NOT show raw token on read (stored hashed): %s", path, bodyStr)
	}
	if !strings.Contains(bodyStr, "Heartbeat-пинг") {
		t.Fatalf("GET %s (owner) missing 'Heartbeat-пинг' section: %s", path, bodyStr)
	}

	// Произвольный диапазон графика задержек (?period=custom&start&end):
	// handler parseTimeRange/autoStep для латентности + селектор в режиме custom.
	custQ := path + "?period=custom&start=2026-07-01T00:00&end=2026-07-10T00:00"
	resp = getWithCookie(t, s.srv, custQ, ownerCookie)
	cbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", custQ, resp.StatusCode, cbody)
	}
	if !strings.Contains(string(cbody), `value="custom" selected`) {
		t.Fatalf("GET %s did not render custom range selected: %s", custQ, cbody)
	}

	// Member (view access via team — с задачи 2 тоже оператор) GET -> 200:
	// секцию Heartbeat-пинг теперь видит (canOperate), но сырой токен на
	// обычном GET не видит никто, независимо от роли.
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
	if !strings.Contains(bodyStr, "Heartbeat-пинг") {
		t.Fatalf("GET %s (member) must show Heartbeat-пинг section (Task 2: canOperate-gated): %s", path, bodyStr)
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
		"name":               {"TCP monitor with bad port"},
		"kind":               {"tcp"},
		"tcp_host":           {"example.com"},
		"tcp_port":           {"999999"}, // Out of range (max 65535)
		"interval_seconds":   {"60"},
		"timeout_seconds":    {"10"},
		"fail_threshold":     {"1"},
		"recovery_threshold": {"1"},
		"consensus":          {"any"},
	}

	resp := postForm(t, s.srv, createPath, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (tcp_port=999999) status = %d, want 422, body: %s", createPath, resp.StatusCode, body)
	}
}
