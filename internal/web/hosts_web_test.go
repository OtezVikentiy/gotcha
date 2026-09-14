package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

type hostsStack struct {
	pool      *pgxpool.Pool
	ch        driver.Conn
	srv       *httptest.Server
	h         *web.Handler
	org       *org.Service
	auth      *auth.Service
	hosts     *host.Store
	incidents *host.IncidentService
	settings  *host.SettingsService
	overrides *host.HostOverrideService
	groups    *host.GroupThresholdService
}

type fakeHostForgetter struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeHostForgetter) Forget(projectID int64, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strconv.FormatInt(projectID, 10)+":"+name)
}

func (f *fakeHostForgetter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newHostsStack(t *testing.T, wireMetrics bool) *hostsStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	hostsStore := host.NewStore(pool)
	hostIncidents := host.NewIncidentService(pool)
	hostSettings := host.NewSettingsService(pool)
	hostOverrides := host.NewHostOverrideService(pool)
	groupThresholds := host.NewGroupThresholdService(pool)
	if wireMetrics {
		h.Metrics = metric.NewQuery(ch)
		h.Hosts = hostsStore
		h.HostIncidents = hostIncidents
		h.HostSettings = hostSettings
		h.HostOverrides = hostOverrides
		h.GroupThresholds = groupThresholds
	}
	h.Register(mux)
	return &hostsStack{
		pool: pool, ch: ch, srv: srv, h: h, org: orgSvc, auth: authSvc,
		hosts: hostsStore, incidents: hostIncidents, settings: hostSettings,
		overrides: hostOverrides, groups: groupThresholds,
	}
}

// Копия неэкспортируемой seedGaugeHost из internal/metric — web_test не может её импортировать.
func (s *hostsStack) seedGaugeHost(t *testing.T, projectID int64, name, hostName string, ts time.Time, val float64, attrs map[string]string) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	if err := s.ch.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, host, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'gauge', '1', 'api', 'prod', ?, ?, ?, ?, 0, [], [], 0, '')`,
		projectID, name, hostName, attrs, ts, val); err != nil {
		t.Fatalf("seed gauge host: %v", err)
	}
}

// Store не даёт менять last_seen напрямую (в проде это делает только Toucher/ingest) —
// нужно для детерминированного «тихого» хоста в тесте.
func (s *hostsStack) setHostLastSeen(t *testing.T, projectID int64, name string, ts time.Time) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE hosts SET last_seen = $1 WHERE project_id = $2 AND name = $3", ts, projectID, name); err != nil {
		t.Fatalf("set last_seen: %v", err)
	}
}

func TestWebHostsList(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "h-co", "H Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "h-proj", "H Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	keys, err := s.org.CreateKeys(ctx, project.ID, org.KindAgent)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	key := keys[0]

	now := time.Now().UTC()

	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-ok"}}); err != nil {
		t.Fatalf("upsert web-ok: %v", err)
	}
	s.seedGaugeHost(t, project.ID, "system.cpu.utilization", "web-ok", now.Add(-time.Minute), 0.20, map[string]string{"state": "idle", "cpu": "0"})
	s.seedGaugeHost(t, project.ID, "system.memory.utilization", "web-ok", now.Add(-time.Minute), 0.55, map[string]string{"state": "used"})
	s.seedGaugeHost(t, project.ID, "system.filesystem.utilization", "web-ok", now.Add(-time.Minute), 0.30, map[string]string{"mountpoint": "/"})
	s.seedGaugeHost(t, project.ID, "system.cpu.load_average.5m", "web-ok", now.Add(-time.Minute), 1.5, nil)
	s.seedGaugeHost(t, project.ID, "system.cpu.logical.count", "web-ok", now.Add(-time.Minute), 3, nil)

	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-disk"}}); err != nil {
		t.Fatalf("upsert web-disk: %v", err)
	}
	diskHost, found, err := s.hosts.Get(ctx, project.ID, "web-disk")
	if err != nil || !found {
		t.Fatalf("get web-disk: found=%v err=%v", found, err)
	}
	if _, _, err := s.incidents.Open(ctx, project.ID, diskHost.ID, "disk", 0.95, "/", false); err != nil {
		t.Fatalf("open disk incident: %v", err)
	}

	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-quiet"}}); err != nil {
		t.Fatalf("upsert web-quiet: %v", err)
	}
	if err := s.settings.Save(ctx, project.ID, host.Settings{
		DiskEnabled: true, DiskThreshold: 0.9,
		MemoryEnabled: true, MemoryThreshold: 0.9,
		LoadEnabled: true, LoadThreshold: 2.0,
		SilentEnabled: true, SilentAfter: host.MinSilentAfter,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	s.setHostLastSeen(t, project.ID, "web-quiet", now.Add(-10*time.Minute))

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)
	for _, name := range []string{"web-ok", "web-disk", "web-quiet"} {
		if !strings.Contains(text, name) {
			t.Errorf("список не содержит хост %q: %s", name, text)
		}
	}
	for _, want := range []string{"80%", "55%", "30%", "0.50"} {
		if !strings.Contains(text, want) {
			t.Errorf("список не содержит значение %q: %s", want, text)
		}
	}
	if !strings.Contains(text, "Норма") {
		t.Errorf("нет бейджа «Норма» (web-ok): %s", text)
	}
	if !strings.Contains(text, "Диск") {
		t.Errorf("нет бейджа вида инцидента «Диск» (web-disk): %s", text)
	}
	if !strings.Contains(text, "Тихий") {
		t.Errorf("нет бейджа «Тихий» (web-quiet): %s", text)
	}
	if !strings.Contains(text, "Bearer "+key.PublicKey) {
		t.Errorf("непустой список без конфига коллектора (нет Bearer с ключом проекта): %s", text)
	}
	if !strings.Contains(text, `<pre class="copy-preview">`) {
		t.Errorf("конфиг коллектора не отрисован видимым блоком: %s", text)
	}

	_, outsider := orgSettingsRegister(t, s.auth, "hosts-outsider@example.com")
	resp = getWithCookie(t, s.srv, base, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}
}

func TestWebHostsListAgentDistUnavailable(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-dist-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "h-dist-co", "H Dist Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "h-dist-proj", "H Dist Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.org.CreateKeys(ctx, project.ID, org.KindAgent); err != nil {
		t.Fatalf("create key: %v", err)
	}

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)
	if strings.Contains(text, "curl -fsSL") {
		t.Errorf("онбординг предлагает install.sh-команду при недоступной раздаче агента: %s", text)
	}
	if !strings.Contains(text, "otlphttp") {
		t.Errorf("коллектор-альтернатива должна остаться заполненной без раздачи агента: %s", text)
	}
}

// Второй Handler на отдельном сервере, чтобы не трогать h.BaseURL исходного стенда.
func TestWebHostsListAgentInsecureBaseURL(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()

	mux2 := http.NewServeMux()
	srv2 := httptest.NewServer(mux2)
	t.Cleanup(srv2.Close)
	h2 := web.New(s.auth, s.org, nil, nil, "http://gotcha.example") // http://, не localhost
	h2.AgentDistDir = t.TempDir()                                   // раздача доступна: изолируем небезопасный BaseURL
	h2.Metrics = metric.NewQuery(s.ch)
	h2.Hosts = s.hosts
	h2.HostIncidents = s.incidents
	h2.HostSettings = s.settings
	h2.Register(mux2)

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-insecure-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "h-insecure-co", "H Insecure Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "h-insecure-proj", "H Insecure Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.org.CreateKeys(ctx, project.ID, org.KindAgent); err != nil {
		t.Fatalf("create key: %v", err)
	}

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, srv2, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)
	if strings.Contains(text, "curl -fsSL") {
		t.Errorf("онбординг предлагает install.sh-команду по незащищённому http:// BaseURL: %s", text)
	}
	if !strings.Contains(text, "otlphttp") {
		t.Errorf("коллектор-альтернатива должна остаться заполненной при небезопасном BaseURL: %s", text)
	}
}

// Хост, тихий по открытому incident kind="silent", и хост, тихий только по last_seen,
// обязаны получить один и тот же бейдж «Тихий», а не мигать в «Тишина» на тике оценщика.
func TestWebHostsSilentBadgeConsistentAcrossSources(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-silentmix-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hsm-co", "HSM Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hsm-proj", "HSM Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Now().UTC()

	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-silent-incident"}}); err != nil {
		t.Fatalf("upsert web-silent-incident: %v", err)
	}
	incidentHost, found, err := s.hosts.Get(ctx, project.ID, "web-silent-incident")
	if err != nil || !found {
		t.Fatalf("get web-silent-incident: found=%v err=%v", found, err)
	}
	if _, _, err := s.incidents.Open(ctx, project.ID, incidentHost.ID, "silent", 0, "", false); err != nil {
		t.Fatalf("open silent incident: %v", err)
	}

	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-silent-lastseen"}}); err != nil {
		t.Fatalf("upsert web-silent-lastseen: %v", err)
	}
	if err := s.settings.Save(ctx, project.ID, host.Settings{
		DiskEnabled: true, DiskThreshold: 0.9,
		MemoryEnabled: true, MemoryThreshold: 0.9,
		LoadEnabled: true, LoadThreshold: 2.0,
		SilentEnabled: true, SilentAfter: host.MinSilentAfter,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	s.setHostLastSeen(t, project.ID, "web-silent-lastseen", now.Add(-10*time.Minute))

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)
	if got := strings.Count(text, "Тихий"); got != 2 {
		t.Errorf("бейдж «Тихий» встречается %d раз(а), want 2 (оба хоста в одном тире): %s", got, text)
	}
	// "Тишина" — текст вида проблемного инцидента (hosts.kind.silent), появляется только
	// если kind="silent" по ошибке попал в OpenKinds тира "problem".
	if strings.Contains(text, "Тишина") {
		t.Errorf("бейдж «Тишина» (тир problem) не должен появляться — silent сворачивается в один тир: %s", text)
	}
}

// Строка хоста в таблице — один <tr>...</tr>; ищем по нему, а не по всему телу, иначе
// бейдж соседнего хоста в другой строке дал бы ложное совпадение.
func hostRowText(t *testing.T, body, hostName string) string {
	t.Helper()
	for _, row := range strings.Split(body, "<tr>") {
		if strings.Contains(row, ">"+hostName+"<") {
			return row
		}
	}
	t.Fatalf("строка хоста %q не найдена в списке: %s", hostName, body)
	return ""
}

func TestWebHostsListSilentBadgeFollowsCascade(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	stale := time.Now().UTC().Add(-1 * time.Hour)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-cascade-owner@example.com")

	// P1: проектная настройка «тишина» ВЫКЛЮЧЕНА — оверрайд хоста должен её включить,
	// хост без оверрайда остаётся здоровым.
	o1, err := s.org.CreateOrg(ctx, "hcasc1-co", "HCasc1 Co", ownerID)
	if err != nil {
		t.Fatalf("create org 1: %v", err)
	}
	p1, err := s.org.CreateProject(ctx, o1.ID, "hcasc1-proj", "HCasc1 Proj", "go")
	if err != nil {
		t.Fatalf("create project 1: %v", err)
	}
	if err := s.settings.Save(ctx, p1.ID, host.Settings{
		DiskEnabled: true, DiskThreshold: 0.9,
		MemoryEnabled: true, MemoryThreshold: 0.9,
		LoadEnabled: true, LoadThreshold: 2.0,
		SilentEnabled: false, SilentAfter: host.MinSilentAfter,
	}); err != nil {
		t.Fatalf("save settings p1: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, p1.ID, []host.TouchEntry{{Name: "override-on"}, {Name: "no-override-off"}}); err != nil {
		t.Fatalf("upsert hosts p1: %v", err)
	}
	overrideOnHost, ok, err := s.hosts.Get(ctx, p1.ID, "override-on")
	if err != nil || !ok {
		t.Fatalf("get override-on: ok=%v err=%v", ok, err)
	}
	silentOn := true
	silentOnAfter := host.MinSilentAfter
	if err := s.overrides.Save(ctx, overrideOnHost.ID, host.ThresholdOverride{
		SilentEnabled: &silentOn, SilentAfter: &silentOnAfter,
	}); err != nil {
		t.Fatalf("save override (silent on): %v", err)
	}
	s.setHostLastSeen(t, p1.ID, "override-on", stale)
	s.setHostLastSeen(t, p1.ID, "no-override-off", stale)

	listPath1 := "/projects/" + strconv.FormatInt(p1.ID, 10) + "/hosts"
	resp := getWithCookie(t, s.srv, listPath1, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", listPath1, resp.StatusCode, body)
	}
	text := string(body)
	if row := hostRowText(t, text, "override-on"); !strings.Contains(row, "Тихий") {
		t.Errorf("проект «тишина» выкл, оверрайд хоста вкл: бейдж не «Тихий»: %s", row)
	}
	if row := hostRowText(t, text, "no-override-off"); strings.Contains(row, "Тихий") {
		t.Errorf("хост без оверрайда при выключенной проектной «тишине» показывает «Тихий»: %s", row)
	}

	// P2: проектная настройка «тишина» ВКЛЮЧЕНА — оверрайд хоста должен её выключить,
	// хост без оверрайда наследует проектную (включённую).
	o2, err := s.org.CreateOrg(ctx, "hcasc2-co", "HCasc2 Co", ownerID)
	if err != nil {
		t.Fatalf("create org 2: %v", err)
	}
	p2, err := s.org.CreateProject(ctx, o2.ID, "hcasc2-proj", "HCasc2 Proj", "go")
	if err != nil {
		t.Fatalf("create project 2: %v", err)
	}
	if err := s.settings.Save(ctx, p2.ID, host.Settings{
		DiskEnabled: true, DiskThreshold: 0.9,
		MemoryEnabled: true, MemoryThreshold: 0.9,
		LoadEnabled: true, LoadThreshold: 2.0,
		SilentEnabled: true, SilentAfter: host.MinSilentAfter,
	}); err != nil {
		t.Fatalf("save settings p2: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, p2.ID, []host.TouchEntry{{Name: "override-off"}, {Name: "no-override-on"}}); err != nil {
		t.Fatalf("upsert hosts p2: %v", err)
	}
	overrideOffHost, ok, err := s.hosts.Get(ctx, p2.ID, "override-off")
	if err != nil || !ok {
		t.Fatalf("get override-off: ok=%v err=%v", ok, err)
	}
	silentOff := false
	if err := s.overrides.Save(ctx, overrideOffHost.ID, host.ThresholdOverride{SilentEnabled: &silentOff}); err != nil {
		t.Fatalf("save override (silent off): %v", err)
	}
	s.setHostLastSeen(t, p2.ID, "override-off", stale)
	s.setHostLastSeen(t, p2.ID, "no-override-on", stale)

	listPath2 := "/projects/" + strconv.FormatInt(p2.ID, 10) + "/hosts"
	resp = getWithCookie(t, s.srv, listPath2, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", listPath2, resp.StatusCode, body)
	}
	text = string(body)
	if row := hostRowText(t, text, "override-off"); strings.Contains(row, "Тихий") {
		t.Errorf("проект «тишина» вкл, оверрайд хоста выкл: бейдж всё равно «Тихий»: %s", row)
	}
	if row := hostRowText(t, text, "no-override-on"); !strings.Contains(row, "Тихий") {
		t.Errorf("хост без оверрайда при включённой проектной «тишине» не показывает «Тихий»: %s", row)
	}
}

func TestWebHostsListNilMetrics(t *testing.T) {
	s := newHostsStack(t, false)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-nil-owner@example.com")
	o, _ := s.org.CreateOrg(ctx, "hn-co", "HN Co", ownerID)
	project, _ := s.org.CreateProject(ctx, o.ID, "hn-proj", "HN Proj", "go")
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil Metrics status = %d, want 404", resp.StatusCode)
	}
}

func TestWebHostsListNilHostsStore(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	h.Metrics = metric.NewQuery(ch) // Hosts/HostIncidents/HostSettings нарочно оставлены nil
	h.Register(mux)

	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "hosts-nilstore-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "hns-co", "HNS Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := orgSvc.CreateProject(ctx, o.ID, "hns-proj", "HNS Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, srv, base, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("Hosts==nil status = %d, want 404 (not panic)", resp.StatusCode)
	}
}

func TestWebHostsListEmptyState(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-empty-owner@example.com")
	o, _ := s.org.CreateOrg(ctx, "he-co", "HE Co", ownerID)
	project, _ := s.org.CreateProject(ctx, o.ID, "he-proj", "HE Proj", "go")
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", base, resp.StatusCode)
	}
	if !strings.Contains(string(body), "Хостов пока нет") {
		t.Fatalf("пустой список без онбординг-заголовка: %s", body)
	}
}

// ServeMux (Go 1.22): литеральный сегмент "settings" выигрывает у {name} независимо от
// порядка регистрации, даже когда в проекте реально есть хост с именем "settings".
func TestWebHostsSettingsBeatsHostNamedSettings(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-settings-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hs-co", "HS Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hs-proj", "HS Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "settings"}}); err != nil {
		t.Fatalf("upsert host named settings: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	text := string(body)
	if !strings.Contains(text, `class="host-settings"`) || !strings.Contains(text, "Пороги хостов") {
		t.Fatalf("тело не похоже на страницу настроек порогов (settings-хендлер): %s", text)
	}
	if strings.Contains(text, `data-chart="`) {
		t.Fatalf("тело содержит маркеры карточки хоста (hostDetail) — {name} выиграл специфичность у settings: %s", text)
	}
}

func TestWebHostSettingsGate(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, _ := orgSettingsRegister(t, s.auth, "hset-gate-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hsg-co", "HSG Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hsg-proj", "HSG Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	_, outsider := orgSettingsRegister(t, s.auth, "hset-gate-outsider@example.com")

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	resp := getWithCookie(t, s.srv, path, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider GET status = %d, want 404", resp.StatusCode)
	}

	resp = postForm(t, s.srv, path, url.Values{"disk_threshold": {"50"}}, s.srv.URL, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider POST status = %d, want 404", resp.StatusCode)
	}
}

// Без Hosts/HostOverrides каскад не посчитать — отказ обязан быть явным (404), а не
// тихим откатом к закрытию инцидентов по всему проекту.
func TestWebHostSettingsSaveGateRequiresCascadeDeps(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	h.Metrics = metric.NewQuery(ch)
	h.HostSettings = host.NewSettingsService(pool)
	h.HostIncidents = host.NewIncidentService(pool)
	// Hosts/HostOverrides нарочно оставлены nil.
	h.Register(mux)

	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "hset-nodeps-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "hsnd-co", "HSND Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := orgSvc.CreateProject(ctx, o.ID, "hsnd-proj", "HSND Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Полная валидная форма — иначе parseHostSettingsForm вернёт 422 раньше, чем
	// дойдёт до гейта зависимостей каскада, и мутация гейта осталась бы незамеченной.
	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	form := url.Values{
		"disk_threshold": {"90"},
		"memory_enabled": {"1"}, "memory_threshold": {"90"},
		"load_enabled": {"1"}, "load_threshold": {"2"},
		"silent_enabled": {"1"}, "silent_after": {"5"},
	}
	resp := postForm(t, srv, path, form, srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("без Hosts/HostOverrides status = %d, want 404 (не тихий откат к закрытию по всему проекту)", resp.StatusCode)
	}
}

func TestWebHostSettingsSaveFlow(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hset-save-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hss-co", "HSS Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hss-proj", "HSS Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	for _, want := range []string{`value="90"`, `value="2"`, `value="5"`} {
		if !strings.Contains(text, want) {
			t.Errorf("GET без сохранённых настроек не отдаёт дефолт %q: %s", want, text)
		}
	}
	assertThresholdGrid(t, text, "host-settings-form")

	validForm := url.Values{
		"disk_enabled": {"1"}, "disk_threshold": {"50"},
		"memory_enabled": {"1"}, "memory_threshold": {"90"},
		"load_enabled": {"1"}, "load_threshold": {"2"},
		"silent_enabled": {"1"}, "silent_after": {"4"},
	}

	resp = postForm(t, s.srv, path, validForm, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}
	if got, err := s.settings.Get(ctx, project.ID); err != nil || got.DiskThreshold != host.DefaultSettings().DiskThreshold {
		t.Fatalf("настройки изменились без Origin: %+v, err=%v", got, err)
	}

	resp = postForm(t, s.srv, path, validForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid POST status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != path {
		t.Fatalf("Location = %q, want %q", loc, path)
	}
	if !hasFlashCookie(resp, "ok|flash.saved") {
		t.Errorf("после сохранения порогов нет flash-cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	saved, err := s.settings.Get(ctx, project.ID)
	if err != nil {
		t.Fatalf("get saved settings: %v", err)
	}
	if saved.DiskThreshold != 0.50 {
		t.Errorf("DiskThreshold = %v, want 0.50", saved.DiskThreshold)
	}
	if saved.SilentAfter != 240*time.Second {
		t.Errorf("SilentAfter = %v, want 240s", saved.SilentAfter)
	}

	invalidForm := url.Values{
		"disk_enabled": {"1"}, "disk_threshold": {"50"},
		"memory_enabled": {"1"}, "memory_threshold": {"90"},
		"load_enabled": {"1"}, "load_threshold": {"2"},
		"silent_enabled": {"1"}, "silent_after": {"2"},
	}
	resp = postForm(t, s.srv, path, invalidForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid silent POST status = %d, want 422: %s", resp.StatusCode, body)
	}
	text = string(body)
	if !strings.Contains(text, `value="2"`) {
		t.Errorf("422-ответ не вернул введённое значение silent_after=2 в форму: %s", text)
	}
	if !strings.Contains(text, "3 минут") {
		t.Errorf("422-ответ без сообщения о границах тишины: %s", text)
	}
	stillSaved, err := s.settings.Get(ctx, project.ID)
	if err != nil {
		t.Fatalf("get settings after invalid POST: %v", err)
	}
	if stillSaved.SilentAfter != 240*time.Second {
		t.Errorf("невалидный POST изменил сохранённый SilentAfter: %v, want 240s (предыдущее валидное значение)", stillSaved.SilentAfter)
	}

	// Без верхней границы 10^12 минут переполнили бы time.Duration и колонку int4 —
	// 500-я вместо 422 с подсказкой.
	overflowForm := url.Values{
		"disk_enabled": {"1"}, "disk_threshold": {"50"},
		"memory_enabled": {"1"}, "memory_threshold": {"90"},
		"load_enabled": {"1"}, "load_threshold": {"2"},
		"silent_enabled": {"1"}, "silent_after": {"1000000000000"},
	}
	resp = postForm(t, s.srv, path, overflowForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("огромный silent_after status = %d, want 422: %s", resp.StatusCode, body)
	}
	afterOverflow, err := s.settings.Get(ctx, project.ID)
	if err != nil {
		t.Fatalf("get settings after overflow POST: %v", err)
	}
	if afterOverflow.SilentAfter != 240*time.Second {
		t.Errorf("POST с переполнением изменил сохранённый SilentAfter: %v, want 240s", afterOverflow.SilentAfter)
	}
}

func TestWebHostSettingsSaveRejectsNaNInf(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hset-naninf-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hsni-co", "HSNI Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hsni-proj", "HSNI Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	baseline := host.Settings{
		DiskEnabled: true, DiskThreshold: 0.60,
		MemoryEnabled: true, MemoryThreshold: 0.70,
		LoadEnabled: true, LoadThreshold: 1.5,
		SilentEnabled: true, SilentAfter: 6 * time.Minute,
	}
	if err := s.settings.Save(ctx, project.ID, baseline); err != nil {
		t.Fatalf("save baseline settings: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	base := url.Values{
		"disk_enabled": {"1"}, "disk_threshold": {"60"},
		"memory_enabled": {"1"}, "memory_threshold": {"70"},
		"load_enabled": {"1"}, "load_threshold": {"1.5"},
		"silent_enabled": {"1"}, "silent_after": {"6"},
	}

	naNForm := url.Values{}
	for k, v := range base {
		naNForm[k] = v
	}
	naNForm.Set("disk_threshold", "NaN")

	resp := postForm(t, s.srv, path, naNForm, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("disk_threshold=NaN status = %d, want 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `value="NaN"`) {
		t.Errorf("422-ответ не вернул введённое значение disk_threshold=NaN в форму: %s", body)
	}
	got, err := s.settings.Get(ctx, project.ID)
	if err != nil {
		t.Fatalf("get settings after NaN POST: %v", err)
	}
	if got.DiskThreshold != baseline.DiskThreshold {
		t.Errorf("NaN POST подменил сохранённый DiskThreshold: %v, want %v (baseline)", got.DiskThreshold, baseline.DiskThreshold)
	}

	infForm := url.Values{}
	for k, v := range base {
		infForm[k] = v
	}
	infForm.Set("load_threshold", "Inf")

	resp = postForm(t, s.srv, path, infForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("load_threshold=Inf status = %d, want 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `value="Inf"`) {
		t.Errorf("422-ответ не вернул введённое значение load_threshold=Inf в форму: %s", body)
	}
	got, err = s.settings.Get(ctx, project.ID)
	if err != nil {
		t.Fatalf("get settings after Inf POST: %v", err)
	}
	if got.LoadThreshold != baseline.LoadThreshold {
		t.Errorf("Inf POST подменил сохранённый LoadThreshold: %v, want %v (baseline)", got.LoadThreshold, baseline.LoadThreshold)
	}
}

func TestWebHostGroupThresholdsFlow(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hgt-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hgt-co", "HGT Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hgt-proj", "HGT Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{
		{Name: "web-1", Environment: "prod", Role: "web"},
	}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	savePath := path + "/groups"
	deletePath := savePath + "/delete"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	if !strings.Contains(text, `href="#new-group-threshold"`) {
		t.Errorf("нет кнопки открытия модалки создания правила: %s", text)
	}
	if !strings.Contains(text, `id="new-group-threshold"`) {
		t.Errorf("модалка создания правила не отрисована: %s", text)
	}
	if strings.Contains(text, "modal--open") {
		t.Errorf("на первом GET не должно быть открытых с сервера модалок: %s", text)
	}
	if !strings.Contains(text, `value="prod"`) || !strings.Contains(text, `value="web"`) {
		t.Errorf("метки хоста (prod/web) не предложены в select: %s", text)
	}

	validForm := url.Values{
		"scope": {"role"}, "label_role": {"web"},
		"disk_mode": {"override"}, "disk_value": {"70"},
		"memory_mode": {"inherit"},
		"load_mode":   {"inherit"},
		"silent_mode": {"inherit"},
	}

	resp = postForm(t, s.srv, savePath, validForm, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}
	if got, err := s.groups.List(ctx, project.ID); err != nil || len(got) != 0 {
		t.Fatalf("правило создано без Origin: %+v, err=%v", got, err)
	}

	_, outsider := orgSettingsRegister(t, s.auth, "hgt-outsider@example.com")
	resp = postForm(t, s.srv, savePath, validForm, s.srv.URL, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider POST status = %d, want 404", resp.StatusCode)
	}

	resp = postForm(t, s.srv, savePath, validForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid POST status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != path {
		t.Fatalf("Location = %q, want %q", loc, path)
	}
	if !hasFlashCookie(resp, "ok|flash.saved") {
		t.Errorf("после сохранения правила нет flash-cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	saved, err := s.groups.List(ctx, project.ID)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(saved) != 1 || saved[0].Scope != "role" || saved[0].Label != "web" {
		t.Fatalf("saved groups = %+v, want one role/web", saved)
	}
	if saved[0].DiskEnabled == nil || !*saved[0].DiskEnabled || saved[0].DiskThreshold == nil || *saved[0].DiskThreshold != 0.70 {
		t.Errorf("disk override = %+v, want enabled=true value=0.70", saved[0].DiskEnabled)
	}

	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if !strings.Contains(text, "70.0%") {
		t.Errorf("таблица правил не показывает заданный порог диска: %s", text)
	}

	editForm := url.Values{
		"scope": {"role"}, "label_role": {"web"},
		"disk_mode": {"override"}, "disk_value": {"55"},
		"memory_mode": {"inherit"},
		"load_mode":   {"inherit"},
		"silent_mode": {"inherit"},
	}
	resp = postForm(t, s.srv, savePath, editForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit POST status = %d, want 303", resp.StatusCode)
	}
	edited, err := s.groups.List(ctx, project.ID)
	if err != nil {
		t.Fatalf("list groups after edit: %v", err)
	}
	if len(edited) != 1 || edited[0].DiskThreshold == nil || *edited[0].DiskThreshold != 0.55 {
		t.Fatalf("edited groups = %+v, want ОДНО правило role/web с disk=0.55 (не вторая строка)", edited)
	}

	invalidForm := url.Values{
		"scope": {"role"}, "label_role": {"web"},
		"disk_mode": {"override"}, "disk_value": {"150"},
		"memory_mode": {"inherit"},
		"load_mode":   {"inherit"},
		"silent_mode": {"inherit"},
	}
	resp = postForm(t, s.srv, savePath, invalidForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid disk POST status = %d, want 422: %s", resp.StatusCode, body)
	}
	text = string(body)
	if !strings.Contains(text, `value="150"`) {
		t.Errorf("422-ответ не вернул введённое значение disk_value=150: %s", text)
	}
	if !strings.Contains(text, "Порог диска должен быть от 1 до 99%") {
		t.Errorf("422-ответ без сообщения о границах диска: %s", text)
	}
	editModalID := templates.EditGroupThresholdModalID("role", "web")
	if !strings.Contains(text, `id="`+editModalID+`" class="modal modal--open"`) {
		t.Errorf("422 правки не переоткрыл модалку правила role/web: %s", text)
	}
	if strings.Contains(text, `id="new-group-threshold" class="modal modal--open"`) {
		t.Errorf("вместо модалки правки правила role/web открылась модалка создания: %s", text)
	}
	stillSaved, err := s.groups.List(ctx, project.ID)
	if err != nil {
		t.Fatalf("list groups after invalid POST: %v", err)
	}
	if len(stillSaved) != 1 || stillSaved[0].DiskThreshold == nil || *stillSaved[0].DiskThreshold != 0.55 {
		t.Errorf("невалидный POST изменил сохранённое правило: %+v, want disk=0.55", stillSaved)
	}

	invalidCreateForm := url.Values{
		"scope": {"env"}, "label_env": {"prod"},
		"disk_mode": {"override"}, "disk_value": {"150"},
		"memory_mode": {"inherit"},
		"load_mode":   {"inherit"},
		"silent_mode": {"inherit"},
	}
	resp = postForm(t, s.srv, savePath, invalidCreateForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid create POST status = %d, want 422: %s", resp.StatusCode, body)
	}
	text = string(body)
	if !strings.Contains(text, `id="new-group-threshold" class="modal modal--open"`) {
		t.Errorf("422 создания не переоткрыл модалку создания: %s", text)
	}
	if strings.Contains(text, `id="`+editModalID+`" class="modal modal--open"`) {
		t.Errorf("422 создания открыл модалку правки чужого правила: %s", text)
	}
	if !strings.Contains(text, `value="55"`) {
		t.Errorf("значения чужой отправки вытеснили значения правила в закрытой модалке правки: %s", text)
	}

	noScopeForm := url.Values{
		"disk_mode": {"inherit"}, "memory_mode": {"inherit"},
		"load_mode": {"inherit"}, "silent_mode": {"inherit"},
	}
	resp = postForm(t, s.srv, savePath, noScopeForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("no-scope POST status = %d, want 422: %s", resp.StatusCode, body)
	}

	delForm := url.Values{"scope": {"role"}, "label": {"web"}}
	resp = postForm(t, s.srv, deletePath, delForm, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete no-origin status = %d, want 403", resp.StatusCode)
	}
	if got, err := s.groups.List(ctx, project.ID); err != nil || len(got) != 1 {
		t.Fatalf("правило удалено без Origin: %+v, err=%v", got, err)
	}

	for _, bad := range []url.Values{
		{"scope": {""}, "label": {"web"}},
		{"scope": {"role"}, "label": {""}},
		{"scope": {"bogus"}, "label": {"web"}},
	} {
		resp = postForm(t, s.srv, deletePath, bad, s.srv.URL, ownerCookie)
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("delete %v status = %d, want 422: %s", bad, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "Выберите окружение или роль и метку из списка") {
			t.Errorf("delete %v: нет сообщения о scope/label: %s", bad, body)
		}
	}
	if got, err := s.groups.List(ctx, project.ID); err != nil || len(got) != 1 {
		t.Fatalf("правило удалено пустой парой: %+v, err=%v", got, err)
	}

	resp = postForm(t, s.srv, deletePath, delForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete (unconfirmed) status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("delete (unconfirmed) missing confirm page hidden field: %s", body)
	}
	if !strings.Contains(string(body), "Роль «web»") {
		t.Fatalf("delete (unconfirmed) confirm page does not name the group: %s", body)
	}
	if !strings.Contains(string(body), `name="scope" value="role"`) || !strings.Contains(string(body), `name="label" value="web"`) {
		t.Fatalf("delete (unconfirmed) confirm page lost scope/label hidden fields: %s", body)
	}
	if got, err := s.groups.List(ctx, project.ID); err != nil || len(got) != 1 {
		t.Fatalf("правило удалено без подтверждения: %+v, err=%v", got, err)
	}

	delForm.Set("confirmed", "yes")
	resp = postForm(t, s.srv, deletePath, delForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete POST status = %d, want 303", resp.StatusCode)
	}
	if !hasFlashCookie(resp, "ok|flash.deleted") {
		t.Errorf("после удаления правила нет flash-cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	afterDelete, err := s.groups.List(ctx, project.ID)
	if err != nil {
		t.Fatalf("list groups after delete: %v", err)
	}
	if len(afterDelete) != 0 {
		t.Errorf("правило не удалено: %+v", afterDelete)
	}
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Групповых правил ещё нет") {
		t.Errorf("страница после удаления не показывает пустой список правил: %s", body)
	}

	resp = postForm(t, s.srv, deletePath, delForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("repeat delete POST status = %d, want 303", resp.StatusCode)
	}
}

func TestWebHostGroupThresholdEditModalsPerRow(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hgtrows-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hgtrows-co", "HGTRows Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hgtrows-proj", "HGTRows Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{
		{Name: "web-1", Environment: "prod", Role: "web"},
	}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	savePath := path + "/groups"
	for _, form := range []url.Values{
		{
			"scope": {"env"}, "label_env": {"prod"},
			"disk_mode": {"override"}, "disk_value": {"70"},
			"memory_mode": {"inherit"}, "load_mode": {"inherit"}, "silent_mode": {"inherit"},
		},
		{
			"scope": {"role"}, "label_role": {"web"},
			"disk_mode": {"override"}, "disk_value": {"55"},
			"memory_mode": {"inherit"}, "load_mode": {"inherit"}, "silent_mode": {"inherit"},
		},
	} {
		resp := postForm(t, s.srv, savePath, form, s.srv.URL, ownerCookie)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("seed POST %v status = %d, want 303", form, resp.StatusCode)
		}
	}

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)

	envID := templates.EditGroupThresholdModalID("env", "prod")
	roleID := templates.EditGroupThresholdModalID("role", "web")
	for _, id := range []string{envID, roleID} {
		if !strings.Contains(text, `id="`+id+`" class="modal"`) {
			t.Errorf("нет закрытой модалки правки %q: %s", id, text)
		}
	}
	for _, id := range []string{envID, roleID} {
		if !strings.Contains(text, `<a class="btn btn-ghost" href="#`+id+`"`) {
			t.Errorf("нет кнопки правки btn-ghost для %q: %s", id, text)
		}
		if strings.Contains(text, `<a href="#`+id+`"`) {
			t.Errorf("правка %q осталась текстовой ссылкой: %s", id, text)
		}
	}
	if !strings.Contains(text, `class="row-actions"`) {
		t.Errorf("действия строки правил без обёртки row-actions: %s", text)
	}
	for _, want := range []string{
		`aria-label="Редактировать правило: Окружение prod"`,
		`aria-label="Удалить правило: Окружение prod"`,
		`aria-label="Редактировать правило: Роль web"`,
		`aria-label="Удалить правило: Роль web"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("нет aria-пояснения %q: %s", want, text)
		}
	}
	if !strings.Contains(text, `type="hidden" name="scope" value="env"`) ||
		!strings.Contains(text, `type="hidden" name="label_env" value="prod"`) {
		t.Errorf("модалка env/prod не несёт свою пару hidden-полями: %s", text)
	}
	if !strings.Contains(text, `type="hidden" name="scope" value="role"`) ||
		!strings.Contains(text, `type="hidden" name="label_role" value="web"`) {
		t.Errorf("модалка role/web не несёт свою пару hidden-полями: %s", text)
	}
	if !strings.Contains(text, `value="70"`) || !strings.Contains(text, `value="55"`) {
		t.Errorf("модалки правки не предзаполнены значениями своих правил (70 и 55): %s", text)
	}
	if got := strings.Count(text, "modal-card--wide"); got != 3 {
		t.Errorf("широких модалок порогов = %d, want 3 (создание + 2 правки)", got)
	}

	idRe := regexp.MustCompile(` id="([^"]+)"`)
	seen := map[string]bool{}
	for _, m := range idRe.FindAllStringSubmatch(text, -1) {
		if seen[m[1]] {
			t.Errorf("дублирующийся id=%q в документе", m[1])
		}
		seen[m[1]] = true
	}
}

func TestWebHostGroupThresholdLegacyEditLink(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hgtlink-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hgtlink-co", "HGTLink Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hgtlink-proj", "HGTLink Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{
		{Name: "web-1", Environment: "prod", Role: "web"},
	}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	form := url.Values{
		"scope": {"role"}, "label_role": {"web"},
		"disk_mode": {"override"}, "disk_value": {"70"},
		"memory_mode": {"inherit"}, "load_mode": {"inherit"}, "silent_mode": {"inherit"},
	}
	resp := postForm(t, s.srv, path+"/groups", form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("seed POST status = %d, want 303", resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, path+"?gt_scope=role&gt_label=web", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy link GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	text := string(body)
	editID := templates.EditGroupThresholdModalID("role", "web")
	if !strings.Contains(text, `id="`+editID+`" class="modal modal--open"`) {
		t.Errorf("старая ссылка не открыла модалку правки role/web: %s", text)
	}
	if strings.Contains(text, `id="new-group-threshold" class="modal modal--open"`) {
		t.Errorf("старая ссылка открыла модалку создания: %s", text)
	}

	resp = getWithCookie(t, s.srv, path+"?gt_scope=env&gt_label=ghost", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ghost pair GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "modal--open") {
		t.Errorf("несуществующая пара открыла модалку: %s", body)
	}
}

func TestWebHostGroupThresholdScopeLabelValidation(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hgtval-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hgtval-co", "HGTVal Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hgtval-proj", "HGTVal Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	savePath := path + "/groups"
	deletePath := savePath + "/delete"

	emptyLabelForm := url.Values{
		"scope": {"env"}, "label_env": {""},
		"disk_mode": {"inherit"}, "memory_mode": {"inherit"},
		"load_mode": {"inherit"}, "silent_mode": {"inherit"},
	}
	resp := postForm(t, s.srv, savePath, emptyLabelForm, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty-label POST status = %d, want 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Выберите окружение или роль и метку из списка") {
		t.Errorf("нет сообщения о scope/label: %s", body)
	}
	if got, err := s.groups.List(ctx, project.ID); err != nil || len(got) != 0 {
		t.Fatalf("правило создано с пустым label: %+v, err=%v", got, err)
	}

	tooLong := strings.Repeat("я", 257)
	tooLongForm := url.Values{
		"scope": {"env"}, "label_env": {tooLong},
		"disk_mode": {"inherit"}, "memory_mode": {"inherit"},
		"load_mode": {"inherit"}, "silent_mode": {"inherit"},
	}
	resp = postForm(t, s.srv, savePath, tooLongForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("too-long-label POST status = %d, want 422: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Выберите окружение или роль и метку из списка") {
		t.Errorf("нет сообщения о scope/label для слишком длинной метки: %s", body)
	}
	if got, err := s.groups.List(ctx, project.ID); err != nil || len(got) != 0 {
		t.Fatalf("правило создано со слишком длинным label: %+v, err=%v", got, err)
	}

	exactLen := strings.Repeat("я", 256)
	exactForm := url.Values{
		"scope": {"env"}, "label_env": {exactLen},
		"disk_mode": {"inherit"}, "memory_mode": {"inherit"},
		"load_mode": {"inherit"}, "silent_mode": {"inherit"},
	}
	resp = postForm(t, s.srv, savePath, exactForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("boundary-label POST status = %d, want 303", resp.StatusCode)
	}
	if got, err := s.groups.List(ctx, project.ID); err != nil || len(got) != 1 {
		t.Fatalf("правило с граничным label не создано: %+v, err=%v", got, err)
	}

	_, outsider := orgSettingsRegister(t, s.auth, "hgtval-outsider@example.com")
	delForm := url.Values{"scope": {"env"}, "label": {exactLen}}
	resp = postForm(t, s.srv, deletePath, delForm, s.srv.URL, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider delete POST status = %d, want 404", resp.StatusCode)
	}
	if got, err := s.groups.List(ctx, project.ID); err != nil || len(got) != 1 {
		t.Fatalf("правило удалено чужим: %+v, err=%v", got, err)
	}
}

func TestWebHostsListStatusSurvivesManyClosedIncidents(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-manyinc-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hmi-co", "HMI Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hmi-proj", "HMI Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-01"}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	hst, ok, err := s.hosts.Get(ctx, project.ID, "web-01")
	if err != nil || !ok {
		t.Fatalf("get host: ok=%v err=%v", ok, err)
	}

	open, _, err := s.incidents.Open(ctx, project.ID, hst.ID, "disk", 0.99, "", false)
	if err != nil {
		t.Fatalf("open disk incident: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		"UPDATE host_incidents SET started_at = now() - interval '1 day' WHERE id = $1", open.ID); err != nil {
		t.Fatalf("состарить открытый инцидент: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, current_value, peak_value, started_at, resolved_at)
		SELECT $1, $2, 'load', 'resolved', 1.5, 1.5, now() - make_interval(secs => g), now()
		FROM generate_series(1, 600) AS g`, project.ID, hst.ID); err != nil {
		t.Fatalf("наполнить закрытыми инцидентами: %v", err)
	}

	resp := getWithCookie(t, s.srv, "/projects/"+strconv.FormatInt(project.ID, 10)+"/hosts", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET списка status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "badge-danger") {
		t.Errorf("открытый инцидент потерялся за 600 закрытыми — хост с живой проблемой показан спокойным: %s", body)
	}
}

func TestWebHostSettingsSaveResolvesDisabledKindIncidents(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hset-disable-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hsd-co", "HSD Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hsd-proj", "HSD Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-01"}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	hst, ok, err := s.hosts.Get(ctx, project.ID, "web-01")
	if err != nil || !ok {
		t.Fatalf("get host: ok=%v err=%v", ok, err)
	}
	if _, _, err := s.incidents.Open(ctx, project.ID, hst.ID, "disk", 0.99, "/snap/core", false); err != nil {
		t.Fatalf("open disk incident: %v", err)
	}
	if _, _, err := s.incidents.Open(ctx, project.ID, hst.ID, "memory", 0.95, "", false); err != nil {
		t.Fatalf("open memory incident: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "badge-danger") {
		t.Fatalf("до выключения порога на списке нет проблемного бейджа: %s", body)
	}

	path := listPath + "/settings"
	form := url.Values{
		"disk_threshold": {"90"},
		"memory_enabled": {"1"}, "memory_threshold": {"90"},
		"load_enabled": {"1"}, "load_threshold": {"2"},
		"silent_enabled": {"1"}, "silent_after": {"5"},
	}
	resp = postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST настроек status = %d, want 303", resp.StatusCode)
	}

	if _, stillOpen, err := s.incidents.OpenFor(ctx, hst.ID, "disk"); err != nil || stillOpen {
		t.Errorf("инцидент выключенного порога «Диск» остался открытым: open=%v err=%v", stillOpen, err)
	}
	if _, stillOpen, err := s.incidents.OpenFor(ctx, hst.ID, "memory"); err != nil || !stillOpen {
		t.Errorf("закрыт инцидент порога «Память», который остался включённым: open=%v err=%v", stillOpen, err)
	}

	all, err := s.incidents.ListByProject(ctx, project.ID, 10)
	if err != nil {
		t.Fatalf("list incidents: %v", err)
	}
	for _, in := range all {
		if in.Kind != "disk" {
			continue
		}
		if in.Status != "resolved" || in.ResolvedAt == nil {
			t.Errorf("disk-инцидент: status=%q resolved_at=%v, want resolved + момент закрытия", in.Status, in.ResolvedAt)
		}
		if in.NotifiedClose {
			t.Errorf("закрытие по выключению порога отправило уведомление (notified_close=true) — это шум о действии самого оператора")
		}
	}
}

func TestWebHostSettingsSaveKeepsOverriddenIncidentOpen(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hset-override-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hso-co", "HSO Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hso-proj", "HSO Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := s.settings.Save(ctx, project.ID, host.Settings{
		DiskEnabled: true, DiskThreshold: 0.9,
		MemoryEnabled: true, MemoryThreshold: 0.9,
		LoadEnabled: true, LoadThreshold: 2.0,
		SilentEnabled: true, SilentAfter: host.MinSilentAfter,
	}); err != nil {
		t.Fatalf("save initial settings: %v", err)
	}

	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-override"}, {Name: "web-plain"}}); err != nil {
		t.Fatalf("upsert hosts: %v", err)
	}
	overridden, ok, err := s.hosts.Get(ctx, project.ID, "web-override")
	if err != nil || !ok {
		t.Fatalf("get web-override: ok=%v err=%v", ok, err)
	}
	plain, ok, err := s.hosts.Get(ctx, project.ID, "web-plain")
	if err != nil || !ok {
		t.Fatalf("get web-plain: ok=%v err=%v", ok, err)
	}

	diskOverrideOn := true
	diskOverrideThreshold := 0.5
	if err := s.overrides.Save(ctx, overridden.ID, host.ThresholdOverride{
		DiskEnabled: &diskOverrideOn, DiskThreshold: &diskOverrideThreshold,
	}); err != nil {
		t.Fatalf("save host override: %v", err)
	}

	if _, _, err := s.incidents.Open(ctx, project.ID, overridden.ID, "disk", 0.95, "", false); err != nil {
		t.Fatalf("open disk incident (host с оверрайдом): %v", err)
	}
	if _, _, err := s.incidents.Open(ctx, project.ID, plain.ID, "disk", 0.95, "", false); err != nil {
		t.Fatalf("open disk incident (host без оверрайда): %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	form := url.Values{
		// disk_enabled опущен — выключаем диск на уровне проекта.
		"disk_threshold": {"90"},
		"memory_enabled": {"1"}, "memory_threshold": {"90"},
		"load_enabled": {"1"}, "load_threshold": {"2"},
		"silent_enabled": {"1"}, "silent_after": {"5"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST настроек status = %d, want 303", resp.StatusCode)
	}

	if _, stillOpen, err := s.incidents.OpenFor(ctx, overridden.ID, "disk"); err != nil || !stillOpen {
		t.Errorf("хост с оверрайдом disk=on: инцидент закрылся сохранением проектной настройки: open=%v err=%v", stillOpen, err)
	}
	if _, stillOpen, err := s.incidents.OpenFor(ctx, plain.ID, "disk"); err != nil || stillOpen {
		t.Errorf("хост без оверрайда: инцидент выключенного порога остался открытым: open=%v err=%v", stillOpen, err)
	}
}

func TestWebHostSettingsSaveKeepsGroupOverriddenIncidentOpen(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hset-group-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hsgr-co", "HSGr Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hsgr-proj", "HSGr Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := s.settings.Save(ctx, project.ID, host.Settings{
		DiskEnabled: true, DiskThreshold: 0.9,
		MemoryEnabled: true, MemoryThreshold: 0.9,
		LoadEnabled: true, LoadThreshold: 2.0,
		SilentEnabled: true, SilentAfter: host.MinSilentAfter,
	}); err != nil {
		t.Fatalf("save initial settings: %v", err)
	}

	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{
		{Name: "db-01", Role: "db"},
		{Name: "web-01", Role: "web"},
	}); err != nil {
		t.Fatalf("upsert hosts: %v", err)
	}
	dbHost, ok, err := s.hosts.Get(ctx, project.ID, "db-01")
	if err != nil || !ok {
		t.Fatalf("get db-01: ok=%v err=%v", ok, err)
	}
	webHost, ok, err := s.hosts.Get(ctx, project.ID, "web-01")
	if err != nil || !ok {
		t.Fatalf("get web-01: ok=%v err=%v", ok, err)
	}

	groupDiskEnabled := true
	groupDiskThreshold := 0.5
	if err := s.groups.Upsert(ctx, project.ID, "role", "db", host.ThresholdOverride{
		DiskEnabled: &groupDiskEnabled, DiskThreshold: &groupDiskThreshold,
	}); err != nil {
		t.Fatalf("save group threshold: %v", err)
	}

	if _, _, err := s.incidents.Open(ctx, project.ID, dbHost.ID, "disk", 0.95, "", false); err != nil {
		t.Fatalf("open disk incident (хост в группе role=db): %v", err)
	}
	if _, _, err := s.incidents.Open(ctx, project.ID, webHost.ID, "disk", 0.95, "", false); err != nil {
		t.Fatalf("open disk incident (хост без группового порога): %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/settings"
	form := url.Values{
		// disk_enabled опущен — выключаем диск на уровне проекта.
		"disk_threshold": {"90"},
		"memory_enabled": {"1"}, "memory_threshold": {"90"},
		"load_enabled": {"1"}, "load_threshold": {"2"},
		"silent_enabled": {"1"}, "silent_after": {"5"},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST настроек status = %d, want 303", resp.StatusCode)
	}

	if _, stillOpen, err := s.incidents.OpenFor(ctx, dbHost.ID, "disk"); err != nil || !stillOpen {
		t.Errorf("хост в группе role=db (групповой порог вкл): инцидент закрылся сохранением проектной настройки: open=%v err=%v", stillOpen, err)
	}
	if _, stillOpen, err := s.incidents.OpenFor(ctx, webHost.ID, "disk"); err != nil || stillOpen {
		t.Errorf("хост без группового порога: инцидент выключенного порога остался открытым: open=%v err=%v", stillOpen, err)
	}
}

func TestWebHostsListEmptyStateOnboardingConfig(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-onboard-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hob-co", "HOB Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hob-proj", "HOB Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	keys, err := s.org.CreateKeys(ctx, project.ID, org.KindAgent)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	key := keys[0]

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)
	if !strings.Contains(text, "endpoint: "+s.srv.URL) {
		t.Errorf("нет endpoint в конфиге онбординга: %s", text)
	}
	// Кавычки внутри textarea в HTML экранированы (&#34;) — корректный текстовый узел,
	// браузер декодирует его обратно в "Bearer <ключ>" при чтении value.
	if !strings.Contains(text, "Bearer "+key.PublicKey) {
		t.Errorf("нет Bearer-заголовка с публичным ключом проекта: %s", text)
	}
	if !strings.Contains(text, `data-copy-format="txt"`) {
		t.Errorf("нет кнопки копирования конфига (copy.js контракт): %s", text)
	}
	if !strings.Contains(text, `<pre class="copy-preview">`) {
		t.Errorf("конфиг коллектора не отрисован видимым блоком: %s", text)
	}
}

// Ключ типа agent жжёт квоту всей организации и регистрирует произвольные хосты — участник
// без owner/admin не должен получить его через /hosts, ни в пустом состоянии, ни в списке.
func TestWebHostsListAgentKeyHiddenFromMember(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hosts-keyhide-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "hosts-keyhide-member@example.com")
	o, err := s.org.CreateOrg(ctx, "hkh-co", "HKH Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hkh-proj", "HKH Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := s.org.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	addTeamAccess(t, s.org, o.ID, project.ID, memberID, "hkh-team")
	keys, err := s.org.CreateKeys(ctx, project.ID, org.KindAgent)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	key := keys[0]

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"

	// Пустое состояние (нет ни одного хоста) — онбординг hostsOnboarding.
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner, empty) status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), key.PublicKey) {
		t.Errorf("GET %s (owner, empty) должен видеть ключ агента: %s", base, body)
	}

	resp = getWithCookie(t, s.srv, base, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (member, empty) status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)
	if strings.Contains(text, key.PublicKey) {
		t.Errorf("GET %s (member, empty) видит ключ агента: %s", base, text)
	}
	if !strings.Contains(text, "видит только owner/admin") {
		t.Errorf("GET %s (member, empty) не показывает подсказку о скрытом ключе: %s", base, text)
	}

	// Непустое состояние (есть хост) — hostsCollectorConfigDetails.
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "keyhide-1"}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	resp = getWithCookie(t, s.srv, base, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), key.PublicKey) {
		t.Errorf("GET %s (owner, non-empty) должен видеть ключ агента: %s", base, body)
	}

	resp = getWithCookie(t, s.srv, base, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if strings.Contains(text, key.PublicKey) {
		t.Errorf("GET %s (member, non-empty) видит ключ агента: %s", base, text)
	}
	if !strings.Contains(text, "keyhide-1") {
		t.Errorf("GET %s (member, non-empty) не видит сам список хостов: %s", base, text)
	}
}

func TestWebHostDetail(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hostdetail-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hd-co", "HD Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hd-proj", "HD Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	name := "веб сервер 1"
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: name}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	hst, found, err := s.hosts.Get(ctx, project.ID, name)
	if err != nil || !found {
		t.Fatalf("get host: found=%v err=%v", found, err)
	}
	if _, _, err := s.incidents.Open(ctx, project.ID, hst.ID, "disk", 0.95, "/var", false); err != nil {
		t.Fatalf("open disk incident: %v", err)
	}

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	path := base + "/" + url.PathEscape(name)
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	text := string(body)
	for _, chart := range []string{"cpu", "mem", "disk_usage", "disk_io", "net", "load", "proc"} {
		marker := `data-chart="` + chart + `"`
		if !strings.Contains(text, marker) {
			t.Errorf("нет маркера графика %q: %s", marker, text)
		}
	}
	if !strings.Contains(text, "Диск") {
		t.Errorf("нет блока открытых инцидентов (вид «Диск»): %s", text)
	}
	// Печатается юнитом вида порога (host.ValueLabel), не сырым числом: диск 0.95 — это «95.0%».
	if !strings.Contains(text, "95.0%") {
		t.Errorf("значение инцидента диска не в процентах: %s", text)
	}
	if strings.Contains(text, ">0.95<") {
		t.Errorf("значение инцидента осталось сырой долей: %s", text)
	}
	// У хоста без истории — подсказка строкой, не emptyState: тот делил бы <h3> с <h2>
	// секции, и заголовок «Последние инциденты» шёл бы дважды подряд.
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "hd-no-incidents"}}); err != nil {
		t.Fatalf("upsert host without incidents: %v", err)
	}
	resp = getWithCookie(t, s.srv, base+"/hd-no-incidents", ownerCookie)
	quietBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET host without incidents status = %d, want 200: %s", resp.StatusCode, quietBody)
	}
	quiet := string(quietBody)
	if n := strings.Count(quiet, "Последние инциденты"); n != 1 {
		t.Errorf("заголовок «Последние инциденты» встречается %d раз, ожидался 1: %s", n, quiet)
	}
	if !strings.Contains(quiet, "Инцидентов ещё не было") {
		t.Errorf("нет подсказки пустой истории инцидентов: %s", quiet)
	}

	missing := base + "/no-such-host"
	resp = getWithCookie(t, s.srv, missing, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (missing host) status = %d, want 404", missing, resp.StatusCode)
	}

	_, outsider := orgSettingsRegister(t, s.auth, "hostdetail-outsider@example.com")
	resp = getWithCookie(t, s.srv, path, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider GET %s status = %d, want 404", path, resp.StatusCode)
	}
}

func TestWebHostDetailLogsLink(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hostdetail-logs-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hd-logs-co", "HD Logs Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hd-logs-proj", "HD Logs Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	name := "web-01"
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: name}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/" + name
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	text := string(body)
	if !strings.Contains(text, "/logs?") {
		t.Errorf("нет ссылки на /logs: %s", text)
	}
	if !strings.Contains(text, "attr=res%3Ahost.name%3Aweb-01") {
		t.Errorf("нет url-экранированного attr=res:host.name:web-01: %s", text)
	}
}

func TestWebHostDetailNilDeps(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	h.Metrics = metric.NewQuery(ch)
	h.Hosts = host.NewStore(pool)
	// HostIncidents/HostSettings нарочно оставлены nil.
	h.Register(mux)

	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "hostdetail-nildeps-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "hdnd-co", "HDND Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := orgSvc.CreateProject(ctx, o.ID, "hdnd-proj", "HDND Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/any-name"
	resp := getWithCookie(t, srv, path, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HostIncidents/HostSettings==nil status = %d, want 404 (not panic)", resp.StatusCode)
	}
}

// .threshold-grid оборачивает все четыре fieldset — общая разметка у host-settings-form
// и host-thresholds-form.
func assertThresholdGrid(t *testing.T, body, form string) {
	t.Helper()
	formAt := strings.Index(body, `class="`+form+`"`)
	if formAt < 0 {
		t.Fatalf("нет формы %s: %s", form, body)
	}
	formEnd := strings.Index(body[formAt:], "</form>")
	if formEnd < 0 {
		t.Fatalf("форма %s не закрыта: %s", form, body)
	}
	sub := body[formAt : formAt+formEnd]
	gridAt := strings.Index(sub, `<div class="threshold-grid">`)
	if gridAt < 0 {
		t.Fatalf("в форме %s нет сетки threshold-grid: %s", form, sub)
	}
	if firstFs := strings.Index(sub, "<fieldset"); firstFs >= 0 && firstFs < gridAt {
		t.Errorf("в форме %s fieldset стоит ДО открытия threshold-grid — карточка вне сетки: %s", form, sub)
	}
	btnAt := strings.Index(sub, "<button")
	if btnAt < 0 {
		t.Fatalf("в форме %s нет кнопки сохранения: %s", form, sub)
	}
	inner := sub[gridAt:btnAt]
	if got := strings.Count(inner, "<fieldset"); got != 4 {
		t.Errorf("в форме %s сетка threshold-grid оборачивает %d fieldset, want 4: %s", form, got, inner)
	}
	// Закрытие обёртки: последний </div> до кнопки идёт ПОСЛЕ последнего
	// </fieldset> — иначе сетка закрылась раньше и хвост карточек снаружи.
	if strings.LastIndex(inner, "</div>") < strings.LastIndex(inner, "</fieldset>") {
		t.Errorf("в форме %s threshold-grid закрывается до последнего fieldset: %s", form, inner)
	}
}

func TestWebHostThresholdsSaveFlow(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hthr-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hthr-co", "Hthr Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hthr-proj", "Hthr Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	name := "web-01"
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: name}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	hst, found, err := s.hosts.Get(ctx, project.ID, name)
	if err != nil || !found {
		t.Fatalf("get host: found=%v err=%v", found, err)
	}

	detailPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/" + name
	savePath := detailPath + "/thresholds"

	resp := getWithCookie(t, s.srv, detailPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `class="host-thresholds-form"`) {
		t.Errorf("оператору не показана форма порогов: %s", body)
	}
	assertThresholdGrid(t, string(body), "host-thresholds-form")

	validForm := url.Values{
		"disk_mode": {"override"}, "disk_value": {"50"},
		"memory_mode": {"off"},
		"load_mode":   {"inherit"},
		"silent_mode": {"override"}, "silent_value": {"10"},
	}

	resp = postForm(t, s.srv, savePath, validForm, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}
	if got, err := s.overrides.Get(ctx, hst.ID); err != nil || got.DiskEnabled != nil {
		t.Fatalf("override изменился без Origin: %+v, err=%v", got, err)
	}

	_, outsider := orgSettingsRegister(t, s.auth, "hthr-outsider@example.com")
	resp = postForm(t, s.srv, savePath, validForm, s.srv.URL, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider POST status = %d, want 404", resp.StatusCode)
	}

	resp = postForm(t, s.srv, savePath, validForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid POST status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != detailPath {
		t.Fatalf("Location = %q, want %q", loc, detailPath)
	}
	if !hasFlashCookie(resp, "ok|flash.saved") {
		t.Errorf("после сохранения порогов нет flash-cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	saved, err := s.overrides.Get(ctx, hst.ID)
	if err != nil {
		t.Fatalf("get saved override: %v", err)
	}
	if saved.DiskEnabled == nil || !*saved.DiskEnabled || saved.DiskThreshold == nil || *saved.DiskThreshold != 0.50 {
		t.Errorf("disk override = %+v, want enabled=true value=0.50", saved.DiskEnabled)
	}
	if saved.MemoryEnabled == nil || *saved.MemoryEnabled {
		t.Errorf("memory override enabled = %v, want false (off)", saved.MemoryEnabled)
	}
	if saved.MemoryThreshold != nil {
		t.Errorf("memory override value = %v, want nil (off без значения)", *saved.MemoryThreshold)
	}
	if saved.LoadEnabled != nil {
		t.Errorf("load override enabled = %v, want nil (inherit)", saved.LoadEnabled)
	}
	if saved.SilentEnabled == nil || !*saved.SilentEnabled || saved.SilentAfter == nil || *saved.SilentAfter != 10*time.Minute {
		t.Errorf("silent override = %+v, want enabled=true value=10m", saved.SilentEnabled)
	}

	resp = getWithCookie(t, s.srv, detailPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(body)
	if !strings.Contains(text, "50.0%") {
		t.Errorf("карточка после сохранения не показывает эффективные 50%% диска: %s", text)
	}
	if !strings.Contains(text, "выключено") {
		t.Errorf("карточка после сохранения не показывает «выключено» для памяти: %s", text)
	}

	invalidForm := url.Values{
		"disk_mode": {"override"}, "disk_value": {"150"},
		"memory_mode": {"off"},
		"load_mode":   {"inherit"},
		"silent_mode": {"override"}, "silent_value": {"10"},
	}
	resp = postForm(t, s.srv, savePath, invalidForm, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid disk POST status = %d, want 422: %s", resp.StatusCode, body)
	}
	text = string(body)
	if !strings.Contains(text, `value="150"`) {
		t.Errorf("422-ответ не вернул введённое значение disk_value=150 в форму: %s", text)
	}
	if !strings.Contains(text, "Порог диска должен быть от 1 до 99%") {
		t.Errorf("422-ответ без сообщения о границах диска: %s", text)
	}
	stillSaved, err := s.overrides.Get(ctx, hst.ID)
	if err != nil {
		t.Fatalf("get override after invalid POST: %v", err)
	}
	if stillSaved.DiskThreshold == nil || *stillSaved.DiskThreshold != 0.50 {
		t.Errorf("невалидный POST изменил сохранённый override диска: %+v, want 0.50", stillSaved.DiskThreshold)
	}

	resp = postForm(t, s.srv, "/projects/"+strconv.FormatInt(project.ID, 10)+"/hosts/no-such-host/thresholds", validForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing host POST status = %d, want 404", resp.StatusCode)
	}
}

// silent: 1 минута не переполняет parseHostThresholdsForm (там граница 0..720), но меньше
// host.MinSilentAfter (3 мин) — ошибка возникает на Save, не на разборе формы.
func TestWebHostThresholdsSaveInvalidMemoryLoadSilent(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hthrmls-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hthrmls-co", "Hthrmls Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hthrmls-proj", "Hthrmls Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	name := "web-01"
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: name}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	savePath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/" + name + "/thresholds"

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{
			"memory вне границы",
			url.Values{
				"disk_mode": {"inherit"}, "memory_mode": {"override"}, "memory_value": {"150"},
				"load_mode": {"inherit"}, "silent_mode": {"inherit"},
			},
			"Порог памяти должен быть от 1 до 99%",
		},
		{
			"load не больше 0",
			url.Values{
				"disk_mode": {"inherit"}, "memory_mode": {"inherit"},
				"load_mode": {"override"}, "load_value": {"0"}, "silent_mode": {"inherit"},
			},
			"Порог нагрузки должен быть больше 0",
		},
		{
			"silent меньше 3 минут",
			url.Values{
				"disk_mode": {"inherit"}, "memory_mode": {"inherit"}, "load_mode": {"inherit"},
				"silent_mode": {"override"}, "silent_value": {"1"},
			},
			"Порог тишины — от 3 минут до 12 часов",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := postForm(t, s.srv, savePath, c.form, s.srv.URL, ownerCookie)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), c.want) {
				t.Errorf("нет сообщения %q: %s", c.want, body)
			}
		})
	}
}

func TestWebHostDeleteConfirmFlow(t *testing.T) {
	s := newHostsStack(t, true)
	forgetter := &fakeHostForgetter{}
	s.h.HostForget = forgetter
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hostdel-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hdel-co", "HDel Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hdel-proj", "HDel Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	name := "web-del-1"
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: name}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	deletePath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/" + url.PathEscape(name) + "/delete"

	resp := postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}
	if forgetter.callCount() != 0 {
		t.Fatalf("Forget called on no-origin request: %d", forgetter.callCount())
	}

	_, outsider := orgSettingsRegister(t, s.auth, "hostdel-outsider@example.com")
	resp = postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}
	if _, found, _ := s.hosts.Get(ctx, project.ID, name); !found {
		t.Fatalf("host removed by outsider-denied request")
	}
	if forgetter.callCount() != 0 {
		t.Fatalf("Forget called on outsider-denied request: %d", forgetter.callCount())
	}

	resp = postForm(t, s.srv, deletePath, url.Values{}, s.srv.URL, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unconfirmed status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `name="confirmed" value="yes"`) {
		t.Fatalf("unconfirmed response missing confirm page hidden field: %s", body)
	}
	if _, found, _ := s.hosts.Get(ctx, project.ID, name); !found {
		t.Fatalf("host removed by unconfirmed request")
	}
	if forgetter.callCount() != 0 {
		t.Fatalf("Forget called on unconfirmed request: %d", forgetter.callCount())
	}

	resp = postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirmed status = %d, want 303", resp.StatusCode)
	}
	wantLoc := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	if !hasFlashCookie(resp, "ok|flash.deleted") {
		t.Errorf("после удаления хоста нет flash-cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	if loc := resp.Header.Get("Location"); loc != wantLoc {
		t.Fatalf("Location = %q, want %q", loc, wantLoc)
	}
	if _, found, _ := s.hosts.Get(ctx, project.ID, name); found {
		t.Fatalf("host still present after confirmed delete")
	}
	if forgetter.callCount() != 1 {
		t.Fatalf("Forget calls = %d, want 1", forgetter.callCount())
	}
}

func TestWebHostDeleteNilHostForget(t *testing.T) {
	s := newHostsStack(t, true) // s.h.HostForget остаётся nil
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "hostdel-nilforget-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "hdnf-co", "HDNF Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "hdnf-proj", "HDNF Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	name := "web-del-nilforget"
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: name}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}

	deletePath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts/" + url.PathEscape(name) + "/delete"
	resp := postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (nil HostForget must not panic)", resp.StatusCode)
	}
	if _, found, _ := s.hosts.Get(ctx, project.ID, name); found {
		t.Fatalf("host still present after confirmed delete")
	}
}

// Значение уходит url.QueryEscape'нутым — сравнивать надо после разэкранирования,
// а не по сырой строке заголовка.
func hasFlashCookie(resp *http.Response, want string) bool {
	for _, c := range resp.Cookies() {
		if c.Name != "flash" {
			continue
		}
		v, err := url.QueryUnescape(c.Value)
		if err == nil && v == want {
			return true
		}
	}
	return false
}
