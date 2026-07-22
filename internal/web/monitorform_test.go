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

// monitorFormStack — own stand (like monitorsStack in monitors_test.go), plus
// h.Alerts (alert.Service): the monitor form's channel checkboxes come from
// Alerts.Channels, which monitorsStack never wires (it doesn't need it — the
// list/detail pages this task doesn't touch alert channels at all).
type monitorFormStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	uptime *uptime.Service
	alerts *alert.Service
	writer *uptime.ResultWriter
}

func newMonitorFormStack(t *testing.T) *monitorFormStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query // страницы мониторов его не используют

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

	return &monitorFormStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, uptime: uptimeSvc, alerts: alertSvc, writer: writer}
}

// ownerAndMember — общий сетап большинства сценариев этого файла: организация
// с owner'ом (может управлять формами монитора) и member'ом с view-доступом
// через команду (не может), плюс сам проект.
func ownerAndMember(t *testing.T, s *monitorFormStack, namePrefix string) (org.Project, *http.Cookie, *http.Cookie) {
	t.Helper()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, namePrefix+"-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, namePrefix+"-member@example.com")

	o, err := s.org.CreateOrg(context.Background(), namePrefix+"-co", namePrefix+" Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, namePrefix+"-proj", namePrefix+" Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	addTeamAccess(t, s.org, o.ID, proj.ID, memberID, namePrefix+"-team")
	return proj, ownerCookie, memberCookie
}

// TestWebMonitorCreateHTTP — вся форма http-монитора со всеми полями:
// проверка, что итоговый монитор в БД получает правильный типизированный
// config, регионы и каналы; успех редиректит на страницу монитора (303).
func TestWebMonitorCreateHTTP(t *testing.T) {
	s := newMonitorFormStack(t)
	proj, ownerCookie, _ := ownerAndMember(t, s, "moncreate")

	channelID, err := s.alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelEmail, Enabled: true, Target: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	newPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors/new"

	// GET new -> 200, форма содержит регион "local" и созданный канал.
	resp := getWithCookie(t, s.srv, newPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", newPath, resp.StatusCode, body)
	}
	for _, want := range []string{"local", "ops@example.com"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing %q: %s", newPath, want, body)
		}
	}

	createPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors"
	form := url.Values{
		"name":                   {"API health"},
		"kind":                   {"http"},
		"http_method":            {"POST"},
		"http_url":               {"https://example.com/health"},
		"http_headers":           {"X-Api-Key: secret\nAccept: application/json"},
		"http_body":              {`{"ping":true}`},
		"http_expected_status":   {"200, 201"},
		"http_body_contains":     {"ok"},
		"http_body_not_contains": {"error"},
		"http_follow_redirects":  {"on"},
		"interval_seconds":       {"90"},
		"timeout_seconds":        {"15"},
		"fail_threshold":         {"2"},
		"recovery_threshold":     {"3"},
		"consensus":              {"all"},
		"remind_every_minutes":   {"30"},
		"ssl_alert_days":         {"7"},
		"regions":                {"local"},
		"channels":               {strconv.FormatInt(channelID, 10)},
	}

	resp = postForm(t, s.srv, createPath, form, s.srv.URL, ownerCookie)
	loc := resp.Header.Get("Location")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303: %s", createPath, resp.StatusCode, body)
	}
	if !strings.HasPrefix(loc, "/monitors/") {
		t.Fatalf("POST %s Location = %q, want /monitors/*", createPath, loc)
	}

	monitors, err := s.uptime.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("list monitors: %v", err)
	}
	if len(monitors) != 1 {
		t.Fatalf("len(monitors) = %d, want 1", len(monitors))
	}
	m := monitors[0]
	if m.Name != "API health" || m.Kind != uptime.KindHTTP {
		t.Fatalf("monitor = %+v, want name=API health kind=http", m)
	}
	if m.IntervalSeconds != 90 || m.TimeoutSeconds != 15 || m.FailThreshold != 2 || m.RecoveryThreshold != 3 {
		t.Fatalf("monitor thresholds = %+v, want 90/15/2/3", m)
	}
	if m.Consensus != uptime.ConsensusAll || m.RemindEveryMinutes != 30 || m.SSLAlertDays != 7 {
		t.Fatalf("monitor consensus/remind/ssl = %+v", m)
	}
	if len(m.Regions) != 1 || m.Regions[0] != "local" {
		t.Fatalf("monitor regions = %v, want [local]", m.Regions)
	}

	full, err := s.uptime.Get(context.Background(), m.ID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if len(full.ChannelIDs) != 1 || full.ChannelIDs[0] != channelID {
		t.Fatalf("monitor channels = %v, want [%d]", full.ChannelIDs, channelID)
	}

	var cfg uptime.HTTPConfig
	if err := json.Unmarshal(full.Config, &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.Method != "POST" || cfg.URL != "https://example.com/health" {
		t.Fatalf("config method/url = %+v", cfg)
	}
	if cfg.Headers["X-Api-Key"] != "secret" || cfg.Headers["Accept"] != "application/json" {
		t.Fatalf("config headers = %+v", cfg.Headers)
	}
	if len(cfg.ExpectedStatus) != 2 || cfg.ExpectedStatus[0] != 200 || cfg.ExpectedStatus[1] != 201 {
		t.Fatalf("config expected_status = %v, want [200 201]", cfg.ExpectedStatus)
	}
	if cfg.BodyContains != "ok" || cfg.BodyNotContains != "error" || !cfg.FollowRedirects {
		t.Fatalf("config body_contains/not_contains/follow_redirects = %+v", cfg)
	}
}

// TestWebMonitorCreateInvalidURLPreservesForm — невалидный http url -> 422, а
// не 500/редирект, и все ранее введённые значения (в т.ч. невалидный url)
// остаются в форме — пользователю не нужно перепечатывать всё заново.
func TestWebMonitorCreateInvalidURLPreservesForm(t *testing.T) {
	s := newMonitorFormStack(t)
	proj, ownerCookie, _ := ownerAndMember(t, s, "moninvalid")

	createPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors"
	form := url.Values{
		"name":               {"Broken monitor"},
		"kind":               {"http"},
		"http_method":        {"GET"},
		"http_url":           {"not-a-valid-url"},
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
		t.Fatalf("POST %s status = %d, want 422: %s", createPath, resp.StatusCode, body)
	}
	for _, want := range []string{"Broken monitor", "not-a-valid-url"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("POST %s (422 body) missing preserved value %q: %s", createPath, want, body)
		}
	}

	monitors, err := s.uptime.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("list monitors: %v", err)
	}
	if len(monitors) != 0 {
		t.Fatalf("len(monitors) = %d, want 0 (invalid create must not persist)", len(monitors))
	}
}

// TestWebMonitorEditChangesFieldsButNotKind — редактирование меняет имя и
// tcp-поля, но kind монитора остаётся прежним, даже если POST пытается
// протащить другое значение kind.
func TestWebMonitorEditChangesFieldsButNotKind(t *testing.T) {
	s := newMonitorFormStack(t)
	proj, ownerCookie, _ := ownerAndMember(t, s, "monedit")

	tcpCfg, err := json.Marshal(uptime.TCPConfig{Host: "old.example.com", Port: 22})
	if err != nil {
		t.Fatalf("marshal tcp config: %v", err)
	}
	created, err := s.uptime.Create(context.Background(), uptime.Monitor{
		ProjectID: proj.ID, Name: "SSH", Kind: uptime.KindTCP, Enabled: true,
		IntervalSeconds: 60, TimeoutSeconds: 10, FailThreshold: 1, RecoveryThreshold: 1,
		Consensus: uptime.ConsensusMajority, Config: tcpCfg,
	}, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	editPath := "/monitors/" + strconv.FormatInt(created.ID, 10) + "/edit"

	// GET edit -> 200, содержит текущее имя/host/port, тип показан как tcp.
	resp := getWithCookie(t, s.srv, editPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", editPath, resp.StatusCode, body)
	}
	for _, want := range []string{"SSH", "old.example.com", "22", "tcp"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing %q: %s", editPath, want, body)
		}
	}

	updatePath := "/monitors/" + strconv.FormatInt(created.ID, 10)
	form := url.Values{
		"name":               {"SSH (renamed)"},
		"kind":               {"http"}, // попытка сменить тип - должна быть проигнорирована
		"tcp_host":           {"new.example.com"},
		"tcp_port":           {"2222"},
		"interval_seconds":   {"120"},
		"timeout_seconds":    {"20"},
		"fail_threshold":     {"1"},
		"recovery_threshold": {"1"},
		"consensus":          {"majority"},
		"regions":            {"local"},
	}
	resp = postForm(t, s.srv, updatePath, form, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303: %s", updatePath, resp.StatusCode, body)
	}

	got, err := s.uptime.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Kind != uptime.KindTCP {
		t.Fatalf("Kind = %q after update, want tcp (must not change)", got.Kind)
	}
	if got.Name != "SSH (renamed)" {
		t.Fatalf("Name = %q, want %q", got.Name, "SSH (renamed)")
	}
	var cfg uptime.TCPConfig
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.Host != "new.example.com" || cfg.Port != 2222 {
		t.Fatalf("config = %+v, want host=new.example.com port=2222", cfg)
	}
	if got.IntervalSeconds != 120 || got.TimeoutSeconds != 20 {
		t.Fatalf("interval/timeout = %d/%d, want 120/20", got.IntervalSeconds, got.TimeoutSeconds)
	}
	if !got.Enabled {
		t.Fatalf("Enabled = false after update, want true (form must not touch enabled)")
	}
}

// TestWebMonitorHeartbeatCreateShowsPingURL — создание heartbeat-монитора
// редиректит на страницу монитора, которая показывает URL пинга с токеном и
// cron-сниппет.
func TestWebMonitorHeartbeatCreateShowsPingURL(t *testing.T) {
	s := newMonitorFormStack(t)
	proj, ownerCookie, _ := ownerAndMember(t, s, "monhb")

	createPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors"
	form := url.Values{
		"name":                    {"Nightly backup"},
		"kind":                    {"heartbeat"},
		"heartbeat_grace_seconds": {"300"},
		"interval_seconds":        {"3600"},
		"timeout_seconds":         {"30"},
		"fail_threshold":          {"1"},
		"recovery_threshold":      {"1"},
		"consensus":               {"any"},
		"regions":                 {"local"},
	}

	// Heartbeat create рендерит деталь СРАЗУ (200) с URL пинга, показанным один
	// раз: сырой токен живёт только в этом ответе (в БД — sha256), redirect его
	// потерял бы. Раньше был 303 + чтение токена из БД.
	resp := postForm(t, s.srv, createPath, form, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200: %s", createPath, resp.StatusCode, body)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, s.srv.URL+"/uptime/hb/") {
		t.Fatalf("POST %s missing ping URL prefix: %s", createPath, bodyStr)
	}
	if !strings.Contains(bodyStr, "curl") {
		t.Fatalf("POST %s missing cron snippet: %s", createPath, bodyStr)
	}

	// Монитор сохранён; Get больше НЕ возвращает сырой токен (хранится хешем).
	monitors, err := s.uptime.List(context.Background(), proj.ID)
	if err != nil {
		t.Fatalf("list monitors: %v", err)
	}
	if len(monitors) != 1 {
		t.Fatalf("len(monitors) = %d, want 1", len(monitors))
	}
	full, err := s.uptime.Get(context.Background(), monitors[0].ID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if full.HeartbeatToken != "" {
		t.Fatalf("HeartbeatToken = %q on read, want empty (hashed at rest)", full.HeartbeatToken)
	}
}

// TestWebMonitorFormMemberForbidden — member (view access, not owner/admin)
// gets 404 on every monitor-form route: both GETs and both POSTs.
func TestWebMonitorFormMemberForbidden(t *testing.T) {
	s := newMonitorFormStack(t)
	proj, ownerCookie, memberCookie := ownerAndMember(t, s, "monforbid")

	existing := baseMonitor(proj.ID, "Existing")
	existing.Config = monHTTPConfig(t, "https://example.com/health")
	created, err := s.uptime.Create(context.Background(), existing, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	newPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors/new"
	createPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/monitors"
	editPath := "/monitors/" + strconv.FormatInt(created.ID, 10) + "/edit"
	updatePath := "/monitors/" + strconv.FormatInt(created.ID, 10)

	resp := getWithCookie(t, s.srv, newPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) status = %d, want 404", newPath, resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, editPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) status = %d, want 404", editPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, createPath, url.Values{"name": {"x"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", createPath, resp.StatusCode)
	}

	resp = postForm(t, s.srv, updatePath, url.Values{"name": {"x"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", updatePath, resp.StatusCode)
	}

	// Sanity: owner still works (rules out an over-broad requireProjectRole check).
	resp = getWithCookie(t, s.srv, newPath, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200", newPath, resp.StatusCode)
	}
}

// TestWebIncidentsList — incidents are visible to a member (view access, not
// just owner/admin — CanAccessProject) with monitor link, cause, and regions;
// outsider gets 404.
func TestWebIncidentsList(t *testing.T) {
	s := newMonitorFormStack(t)
	proj, ownerCookie, memberCookie := ownerAndMember(t, s, "monincident")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "monincident-outsider@example.com")

	flaky := baseMonitor(proj.ID, "Flaky API")
	flaky.Config = monHTTPConfig(t, "https://example.com/health")
	created, err := s.uptime.Create(context.Background(), flaky, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if _, _, err := s.uptime.OpenIncident(context.Background(), created.ID, "connection refused", []string{"local"}, false); err != nil {
		t.Fatalf("open incident: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/incidents"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	for _, want := range []string{"Flaky API", "connection refused", "local", "продолжается"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing %q: %s", path, want, body)
		}
	}

	resp = getWithCookie(t, s.srv, path, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (member) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Flaky API") {
		t.Fatalf("GET %s (member) missing incident: %s", path, body)
	}

	resp = getWithCookie(t, s.srv, path, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", path, resp.StatusCode)
	}
}
