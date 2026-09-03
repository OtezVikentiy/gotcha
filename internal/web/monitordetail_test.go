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

	// Произвольный диапазон в адресе (?period=custom&start&end) страницу
	// heartbeat не ломает: handler parseTimeRange/autoStep отрабатывает, а
	// селектора на странице нет — секция задержки у heartbeat не рендерится
	// (сам селектор в режиме custom проверяется на http-мониторе в
	// TestWebMonitorDetailLatencyTitleFollowsRange).
	custQ := path + "?period=custom&start=2026-07-01T00:00&end=2026-07-10T00:00"
	resp = getWithCookie(t, s.srv, custQ, ownerCookie)
	cbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", custQ, resp.StatusCode, cbody)
	}
	if strings.Contains(string(cbody), `value="custom" selected`) {
		t.Fatalf("GET %s rendered the latency range selector on a heartbeat monitor: %s", custQ, cbody)
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

// monitorDetailProject — org + project для тестов страницы монитора, где
// нужен только owner: возвращает id проекта и cookie владельца.
func monitorDetailProject(t *testing.T, s *monitorDetailStack, slug string) (int64, *http.Cookie) {
	t.Helper()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, slug+"-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), slug+"-co", slug+" Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, slug+"-proj", slug+" Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return proj.ID, ownerCookie
}

// monitorDetailGet — GET страницы монитора с ожиданием 200, тело строкой.
func monitorDetailGet(t *testing.T, s *monitorDetailStack, path string, cookie *http.Cookie) string {
	t.Helper()
	resp := getWithCookie(t, s.srv, path, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	return string(body)
}

// TestWebMonitorDetailHeartbeatTiles — страница heartbeat-монитора собрана
// не под опросный монитор: вместо доступности 24ч/7д/30д и SSL — три плитки
// «Последний маячок / Допуск / Ожидается до» с реальными значениями
// (last_beat_at задан явно, допуск 15 минут → срок = last_beat_at + 15 мин),
// а секций «Задержка» (заголовок, легенда фаз, селектор окна) и «Последние
// проверки» нет вовсе — heartbeat не пишет check_results по конструкции.
// Лента инцидентов остаётся.
func TestWebMonitorDetailHeartbeatTiles(t *testing.T) {
	s := newMonitorDetailStack(t)
	projID, ownerCookie := monitorDetailProject(t, s, "hb-tiles")

	hbConfigJSON, err := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 900})
	if err != nil {
		t.Fatalf("marshal heartbeat config: %v", err)
	}
	m := baseMonitor(projID, "Nightly backup")
	m.Kind = uptime.KindHeartbeat
	m.Config = hbConfigJSON
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create heartbeat monitor: %v", err)
	}
	path := "/monitors/" + strconv.FormatInt(created.ID, 10)

	// Маячка ещё не было: плитки есть, последний — «ещё не было», срок — прочерк.
	body := monitorDetailGet(t, s, path, ownerCookie)
	for _, want := range []string{"Последний маячок", "ещё не было", "Допуск", "15 мин", "Ожидается до", "Инцидентов не было"} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET %s (no beat yet) missing %q: %s", path, want, body)
		}
	}
	if !strings.Contains(body, `<div class="stat-label">Ожидается до</div><div class="stat-value">—</div>`) {
		t.Fatalf("GET %s (no beat yet) must show a dash for the expected-by tile: %s", path, body)
	}
	for _, absent := range []string{
		"Задержка (", "legend-ttfb", `name="period"`, "Последние проверки", "Проверок ещё не было",
		"Доступность 24ч", "Доступность 7д", "Доступность 30д", `<div class="stat-label">SSL</div>`,
	} {
		if strings.Contains(body, absent) {
			t.Fatalf("GET %s (heartbeat) must not render %q: %s", path, absent, body)
		}
	}

	// Маячок был в известный момент: относительное время в <time datetime>,
	// срок — абсолютное время last_beat_at + 15 мин в формате страницы.
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE monitors SET last_beat_at = '2026-07-01 12:00:00+00' WHERE id = $1", created.ID); err != nil {
		t.Fatalf("set last_beat_at: %v", err)
	}
	body = monitorDetailGet(t, s, path, ownerCookie)
	if !strings.Contains(body, `<time datetime="2026-07-01T12:00:00Z"`) {
		t.Fatalf("GET %s missing last-beat <time> for 2026-07-01T12:00:00Z: %s", path, body)
	}
	if strings.Contains(body, "ещё не было") {
		t.Fatalf("GET %s still says 'ещё не было' after a beat: %s", path, body)
	}
	if !strings.Contains(body, `<div class="stat-label">Ожидается до</div><div class="stat-value">2026-07-01 12:15 UTC</div>`) {
		t.Fatalf("GET %s missing expected-by 2026-07-01 12:15 UTC: %s", path, body)
	}
}

// TestWebMonitorDetailHeartbeatGraceCompound — допуск, не кратный часу,
// печатается двумя единицами («1 ч 30 мин»), а не округляется до «1 ч».
func TestWebMonitorDetailHeartbeatGraceCompound(t *testing.T) {
	s := newMonitorDetailStack(t)
	projID, ownerCookie := monitorDetailProject(t, s, "hb-grace")

	hbConfigJSON, err := json.Marshal(uptime.HeartbeatConfig{GraceSeconds: 5400})
	if err != nil {
		t.Fatalf("marshal heartbeat config: %v", err)
	}
	m := baseMonitor(projID, "Hourly sync")
	m.Kind = uptime.KindHeartbeat
	m.Config = hbConfigJSON
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create heartbeat monitor: %v", err)
	}
	path := "/monitors/" + strconv.FormatInt(created.ID, 10)
	body := monitorDetailGet(t, s, path, ownerCookie)
	if !strings.Contains(body, `<div class="stat-label">Допуск</div><div class="stat-value">1 ч 30 мин</div>`) {
		t.Fatalf("GET %s missing grace '1 ч 30 мин': %s", path, body)
	}
}

// TestWebMonitorDetailSSLTileOnlyForHTTP — плитка SSL есть у http-монитора и
// отсутствует у tcp: сертификат проверяет только http, прочерк у tcp читался
// бы как «сертификат не найден».
func TestWebMonitorDetailSSLTileOnlyForHTTP(t *testing.T) {
	s := newMonitorDetailStack(t)
	projID, ownerCookie := monitorDetailProject(t, s, "ssl-tile")

	httpMon := baseMonitor(projID, "Site")
	httpMon.Config = monHTTPConfig(t, "https://example.com/")
	httpCreated, err := s.uptime.Create(context.Background(), httpMon, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create http monitor: %v", err)
	}
	tcpCfg, err := json.Marshal(uptime.TCPConfig{Host: "db.example.com", Port: 5432})
	if err != nil {
		t.Fatalf("marshal tcp config: %v", err)
	}
	tcpMon := baseMonitor(projID, "Database")
	tcpMon.Kind = uptime.KindTCP
	tcpMon.Config = tcpCfg
	tcpCreated, err := s.uptime.Create(context.Background(), tcpMon, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create tcp monitor: %v", err)
	}

	const sslTile = `<div class="stat-label">SSL</div>`
	httpPath := "/monitors/" + strconv.FormatInt(httpCreated.ID, 10)
	if body := monitorDetailGet(t, s, httpPath, ownerCookie); !strings.Contains(body, sslTile) {
		t.Fatalf("GET %s (http) missing SSL tile: %s", httpPath, body)
	}
	tcpPath := "/monitors/" + strconv.FormatInt(tcpCreated.ID, 10)
	body := monitorDetailGet(t, s, tcpPath, ownerCookie)
	if strings.Contains(body, sslTile) {
		t.Fatalf("GET %s (tcp) must not render SSL tile: %s", tcpPath, body)
	}
	// Опросный монитор без heartbeat: плитки доступности и задержка на месте.
	for _, want := range []string{"Доступность 24ч", "Задержка (24 ч)", "Последние проверки"} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET %s (tcp) missing %q: %s", tcpPath, want, body)
		}
	}
}

// TestWebMonitorDetailLatencyTitleFollowsRange — заголовок «Задержка (…)»
// печатает ту же подпись, что и селектор окна: пресет 7d → «7 дн» (range.7d),
// произвольный диапазон → границы в формате страницы.
func TestWebMonitorDetailLatencyTitleFollowsRange(t *testing.T) {
	s := newMonitorDetailStack(t)
	projID, ownerCookie := monitorDetailProject(t, s, "lat-title")

	m := baseMonitor(projID, "API")
	m.Config = monHTTPConfig(t, "https://example.com/api")
	created, err := s.uptime.Create(context.Background(), m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	path := "/monitors/" + strconv.FormatInt(created.ID, 10)

	body := monitorDetailGet(t, s, path+"?period=7d", ownerCookie)
	if !strings.Contains(body, "<h2>Задержка (7 дн)</h2>") {
		t.Fatalf("GET %s?period=7d missing 'Задержка (7 дн)' heading: %s", path, body)
	}
	if strings.Contains(body, "Задержка (24") {
		t.Fatalf("GET %s?period=7d still shows the 24h heading: %s", path, body)
	}

	custQ := path + "?period=custom&start=2026-07-01T00:00&end=2026-07-10T00:00"
	body = monitorDetailGet(t, s, custQ, ownerCookie)
	if !strings.Contains(body, `value="custom" selected`) {
		t.Fatalf("GET %s did not render custom range selected: %s", custQ, body)
	}
	if !strings.Contains(body, "<h2>Задержка (2026-07-01 00:00 UTC – 2026-07-10 00:00 UTC)</h2>") {
		t.Fatalf("GET %s missing custom-range heading: %s", custQ, body)
	}
}
