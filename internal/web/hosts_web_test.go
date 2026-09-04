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

// fakeHostForgetter реализует web.HostForgetter без реального host.Toucher —
// web-тесту hostDelete важен только факт вызова Forget(projectID, name)
// после удаления, не поведение троттлера (это внутренняя логика host.Toucher,
// покрытая internal/host/touch_test.go).
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

// newHostsStack поднимает мигрированные PG+CH и Handler, как newMetricsStack
// (metrics_test.go) — но дополнительно проводит Hosts/HostIncidents/
// HostSettings, как это делает cmd/gotcha/main.go всегда вместе с Metrics.
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

// seedGaugeHost — точка метрики-gauge с host-атрибуцией (как seedGaugeHost в
// internal/metric — неэкспортируемая копия для web_test, который не может её
// импортировать).
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

// setHostLastSeen перематывает last_seen хоста напрямую в PG (Store не даёт
// такой ручки — в проде last_seen двигает только Toucher/ingest) — нужно,
// чтобы детерминированно смоделировать «тихий» хост в тесте.
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

	// web-ok: свежий, без инцидентов — все метрики есть.
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-ok"}}); err != nil {
		t.Fatalf("upsert web-ok: %v", err)
	}
	s.seedGaugeHost(t, project.ID, "system.cpu.utilization", "web-ok", now.Add(-time.Minute), 0.20, map[string]string{"state": "idle", "cpu": "0"})
	s.seedGaugeHost(t, project.ID, "system.memory.utilization", "web-ok", now.Add(-time.Minute), 0.55, map[string]string{"state": "used"})
	s.seedGaugeHost(t, project.ID, "system.filesystem.utilization", "web-ok", now.Add(-time.Minute), 0.30, map[string]string{"mountpoint": "/"})
	s.seedGaugeHost(t, project.ID, "system.cpu.load_average.5m", "web-ok", now.Add(-time.Minute), 1.5, nil)
	s.seedGaugeHost(t, project.ID, "system.cpu.logical.count", "web-ok", now.Add(-time.Minute), 3, nil)

	// web-disk: открытый инцидент диска — статус-бейдж вида "disk".
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

	// web-quiet: last_seen старше порога тишины, без инцидентов.
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
	// Значения метрик web-ok (CPU busy = 1-0.20 = 0.80, RAM 0.55, диск 0.30,
	// load/core = 1.5/3 = 0.50).
	for _, want := range []string{"80%", "55%", "30%", "0.50"} {
		if !strings.Contains(text, want) {
			t.Errorf("список не содержит значение %q: %s", want, text)
		}
	}
	// Статус-бейджи: ok у web-ok, вид инцидента у web-disk, "тихий" у web-quiet.
	if !strings.Contains(text, "Норма") {
		t.Errorf("нет бейджа «Норма» (web-ok): %s", text)
	}
	if !strings.Contains(text, "Диск") {
		t.Errorf("нет бейджа вида инцидента «Диск» (web-disk): %s", text)
	}
	if !strings.Contains(text, "Тихий") {
		t.Errorf("нет бейджа «Тихий» (web-quiet): %s", text)
	}
	// P2-11: конфиг коллектора доступен и с НЕПУСТОГО списка (подключение
	// второго сервера), а не только из онбординга пустого состояния.
	if !strings.Contains(text, "Bearer "+key.PublicKey) {
		t.Errorf("непустой список без конфига коллектора (нет Bearer с ключом проекта): %s", text)
	}
	// P1-1: конфиг виден глазами, а не только «за кнопкой» — проверить
	// подставленные адрес и ключ можно, ничего не копируя.
	if !strings.Contains(text, `<pre class="copy-preview">`) {
		t.Errorf("конфиг коллектора не отрисован видимым блоком: %s", text)
	}

	// Чужой (не член организации) → 404.
	_, outsider := orgSettingsRegister(t, s.auth, "hosts-outsider@example.com")
	resp = getWithCookie(t, s.srv, base, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}
}

// TestWebHostsListAgentDistUnavailable — rem-A sec-M1: активный ключ проекта
// есть (коллектор заполняется), но раздача бинарей агента не сконфигурирована
// (h.AgentDistDir пуст — как newHostsStack поднимает Handler по умолчанию,
// та же ситуация, что на инстансе, собранном не из Docker-образа). Онбординг
// не должен предлагать install.sh-команду: `agentDistAvailable()` лжив, и
// эта команда гарантированно упёрлась бы в 404 на каждом хосте парка.
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

// TestWebHostsListAgentInsecureBaseURL — rem-A sec-M4: BaseURL не https:// и
// не локальный — онбординг не должен отдавать root-команду, которая тянет
// бинарь и SHA256SUMS по каналу, уязвимому MITM. Второй Handler на отдельном
// сервере, чтобы не трогать h.BaseURL исходного стенда (тот httptest-локален
// и остаётся валидным фикстурой для остальных тестов файла).
func TestWebHostsListAgentInsecureBaseURL(t *testing.T) {
	s := newHostsStack(t, true)
	ctx := context.Background()

	mux2 := http.NewServeMux()
	srv2 := httptest.NewServer(mux2)
	t.Cleanup(srv2.Close)
	h2 := web.New(s.auth, s.org, nil, nil, "http://gotcha.example") // http://, не localhost — sec-M4
	h2.AgentDistDir = t.TempDir()                                   // раздача доступна — изолирует именно sec-M4
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

// TestWebHostsSilentBadgeConsistentAcrossSources — регрессия ревью T14
// (находка 2): хост, тихий по факту открытого incident kind="silent"
// (host.Evaluator уже тикнул), и хост, тихий только по last_seen (evaluator
// ещё не тикнул), обязаны получить ОДИН И ТОТ ЖЕ бейдж «Тихий» — не
// «Тишина» (тот текст означает вид ОТКРЫТОГО инцидента среди «проблемных»,
// см. hosts.kind.silent). Разные тексты для одного и того же состояния и
// были бы мерцанием бейджа на тике оценщика без изменения сути.
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

	// web-silent-incident: last_seen СВЕЖИЙ, но есть открытый incident
	// kind="silent" (как будто host.Evaluator уже успел его открыть на
	// предыдущем тике, до того как хост снова "ожил").
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

	// web-silent-lastseen: тихий ТОЛЬКО по last_seen, без единого инцидента
	// (evaluator ещё не тикнул).
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
	// "Тишина" — текст ВИДА проблемного инцидента (hosts.kind.silent),
	// появляется, только если kind="silent" по ошибке попал в OpenKinds
	// (тир "problem"). Его не должно быть вовсе — обе тихих строки не
	// содержат бейджа "problem".
	if strings.Contains(text, "Тишина") {
		t.Errorf("бейдж «Тишина» (тир problem) не должен появляться — silent сворачивается в один тир: %s", text)
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

// TestWebHostsListNilHostsStore — регрессия ревью T14 (находка 1): Metrics
// проставлен, а Hosts/HostIncidents/HostSettings — нет. Инвариант «main.go
// всегда проставляет их вместе с Metrics» на практике уже нарушен другими
// тестовыми стендами (shell_operate_e2e_test.go, authz_behavior_test.go
// вооружают только h.Metrics) — без собственного nil-гейта в hostsList
// авторизованный участник получил бы панику на h.Hosts.List(nil), а не 404.
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

// TestWebHostsSettingsBeatsHostNamedSettings — специфичность маршрута
// ServeMux (Go 1.22): литеральный сегмент "settings" выигрывает у {name}
// независимо от порядка регистрации, даже когда в проекте реально есть хост
// с именем "settings" — по /hosts/settings всегда отвечает
// hostSettingsPage, не hostDetail (см. web.go, комментарий у маршрутов).
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
	// hostSettingsPage отвечает страницей формы порогов (маркер — корневой
	// класс "host-settings" и заголовок nav.host_thresholds, «Пороги
	// хостов»); hostDetail отвечает карточкой хоста с графиками
	// (data-chart="..."). Если бы {name} выиграл специфичность, тело
	// содержало бы маркеры карточки, а не формы настроек.
	text := string(body)
	if !strings.Contains(text, `class="host-settings"`) || !strings.Contains(text, "Пороги хостов") {
		t.Fatalf("тело не похоже на страницу настроек порогов (settings-хендлер): %s", text)
	}
	if strings.Contains(text, `data-chart="`) {
		t.Fatalf("тело содержит маркеры карточки хоста (hostDetail) — {name} выиграл специфичность у settings: %s", text)
	}
}

// TestWebHostSettingsGate — не-член организации получает 404 и на GET, и
// на POST (requireProjectOperator — существования проекта не палит).
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

// TestWebHostSettingsSaveFlow — GET отдаёт дефолты (строки настроек ещё
// нет); POST без Origin → 403 (denyCrossOrigin), настройки не тронуты;
// валидный POST (диск 50%, silent 4 мин) → 303 на ту же страницу, Get
// отдаёт сохранённые 0.50/240s; POST с silent=2 (< MinSilentAfter=180s=3мин)
// → 422 с FormState-ошибкой, введённое значение "2" возвращается в форму
// (не потеряно), настройки в БД не изменены последним невалидным POST.
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

	// GET — дефолты без сохранённой строки (host.DefaultSettings: 90/90/2.0/5мин).
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

	// Без Origin → 403, настройки НЕ сохраняются.
	resp = postForm(t, s.srv, path, validForm, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}
	if got, err := s.settings.Get(ctx, project.ID); err != nil || got.DiskThreshold != host.DefaultSettings().DiskThreshold {
		t.Fatalf("настройки изменились без Origin: %+v, err=%v", got, err)
	}

	// Валидный POST → 303 на страницу настроек, значения сохранены.
	resp = postForm(t, s.srv, path, validForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid POST status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != path {
		t.Fatalf("Location = %q, want %q", loc, path)
	}
	// P1-5: сохранение сообщает о себе — до правки редирект возвращал форму с
	// теми же значениями и выглядел как «ничего не произошло».
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

	// silent=2 мин (< 3 мин минимум) → 422, FormState возвращает введённое
	// значение, сохранённые настройки не подменяются мусором.
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

	// Верхней границы у поля раньше не было: 10^12 минут переполняли и
	// time.Duration, и колонку int4 — пользователь получал 500-ю на опечатке
	// вместо 422 с подсказкой.
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

// TestWebHostSettingsSaveRejectsNaNInf — ревью T16 (Important): strconv.
// ParseFloat принимает "NaN"/"Inf" без ошибки, а host.Validate сравнивает
// порог с границами через </<=/> — сравнение с NaN всегда false в обе
// стороны, поэтому такой порог тихо проходил бы Validate и сохранялся в
// БД (Postgres double precision и CHECK(load_threshold > 0) тоже принимают
// NaN/Infinity), после чего оценщик host.Evaluator никогда бы не срабатывал.
// disk_threshold=NaN и (отдельным POST) load_threshold=Inf обязаны получить
// 422 с FormState, а НЕ подменить ранее сохранённое валидное значение.
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
	// Известное валидное состояние в БД — POST с NaN/Inf не должен его тронуть.
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

// TestWebHostGroupThresholdsFlow — блок «Пороги по окружению/роли» (B2, T7)
// на /hosts/settings: без Origin/чужому оператору — 403/404, ничего не
// меняется; валидный POST scope=role/label=web создаёт правило
// (GroupThresholdService.Upsert), 303 на страницу настроек, flash, строка в
// таблице; повторный POST под ТОЙ ЖЕ парой scope+label — редактирование
// (замещает диск-override другим значением, а не создаёт вторую строку —
// Upsert идемпотентен по (project_id,scope,label)); невалидный POST (диск
// вне границы) → 422 с сообщением и введённым значением, сохранённое правило
// не подменяется мусором; POST без scope/label → 422; удаление — 303,
// правило исчезает из PG и со страницы, повторное удаление идемпотентно.
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

	// GET — форма добавления показана (у проекта есть метки prod/web),
	// групповых правил ещё нет.
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

	// Без Origin → 403, правило не создано.
	resp = postForm(t, s.srv, savePath, validForm, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}
	if got, err := s.groups.List(ctx, project.ID); err != nil || len(got) != 0 {
		t.Fatalf("правило создано без Origin: %+v, err=%v", got, err)
	}

	// Чужой (не член организации) → 404.
	_, outsider := orgSettingsRegister(t, s.auth, "hgt-outsider@example.com")
	resp = postForm(t, s.srv, savePath, validForm, s.srv.URL, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider POST status = %d, want 404", resp.StatusCode)
	}

	// Валидный POST → 303, flash, правило в PG.
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

	// GET после сохранения — таблица показывает строку правила.
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if !strings.Contains(text, "70.0%") {
		t.Errorf("таблица правил не показывает заданный порог диска: %s", text)
	}

	// Повторный POST под той же парой scope+label — редактирование: Upsert
	// замещает диск-override другим значением, а не создаёт вторую строку.
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

	// Невалидный POST (диск вне границы 1..100%) → 422, сообщение + введённое
	// значение, ранее сохранённое правило не подменяется мусором.
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
	// Пара role/web уже существует — 422 обязан переоткрыть модалку правки
	// ИМЕННО этого правила, а не модалку создания (образец —
	// TestWebMaintenanceUpdateInvalidReopensModal).
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

	// Невалидный POST с парой, которой нет среди правил (создание нового) →
	// 422 переоткрывает модалку СОЗДАНИЯ с введённым значением, модалка
	// правки существующего правила остаётся закрытой.
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
	// Введённое при 422 попадает только в ПЕРЕОТКРЫТУЮ модалку: закрытая
	// модалка правки role/web продолжает показывать значение своего правила
	// (диск 55%), а не значения чужой отправки (groupThresholdFormValues).
	if !strings.Contains(text, `value="55"`) {
		t.Errorf("значения чужой отправки вытеснили значения правила в закрытой модалке правки: %s", text)
	}

	// POST без scope/label → 422 (нечего сохранять).
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

	// Удаление без Origin → 403, правило не удалено.
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

	// Валидное удаление → 303, flash, правило исчезает из PG и со страницы.
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

	// Повторное удаление отсутствующей строки — идемпотентно, 303, без ошибки.
	resp = postForm(t, s.srv, deletePath, delForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("repeat delete POST status = %d, want 303", resp.StatusCode)
	}
}

// TestWebHostGroupThresholdEditModalsPerRow — модалок правки столько же,
// сколько строк таблицы, каждая предзаполнена значениями СВОЕГО правила
// (scope+метка hidden-полями, порог — числом правила), и при этом в
// документе нет повторяющихся id: сегмент-контролы и поля повторяются в
// каждой модалке, любой захардкоженный id давал бы дубль, а клик по label
// одной модалки переключал бы radio в другой.
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
	// Пара действий строки — как на «Подавлении шторма»: «Редактировать»
	// вторичной кнопкой (btn-ghost), «Удалить» — btn-danger, оба в обёртке
	// .row-actions; текстовой ссылки-редактирования больше нет.
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
	// Пояснения для скринридера — на обеих кнопках каждой строки, с парой
	// правила (как у suppressionEdgeRow на «Подавлении шторма»).
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
	// Предзаполнение: у каждой модалки пара своего правила hidden-полями и
	// порог диска числом правила (70% у env/prod, 55% у role/web).
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
	// Все модалки порогов — широкие (wide): форма с четырьмя fieldset в
	// узкой карточке сплющивается. Создание + по одной правке на строку.
	if got := strings.Count(text, "modal-card--wide"); got != 3 {
		t.Errorf("широких модалок порогов = %d, want 3 (создание + 2 правки)", got)
	}

	// Дубликаты id в документе: форм на странице несколько, повторяющийся id
	// ломает связку label/for и якоря модалок.
	idRe := regexp.MustCompile(` id="([^"]+)"`)
	seen := map[string]bool{}
	for _, m := range idRe.FindAllStringSubmatch(text, -1) {
		if seen[m[1]] {
			t.Errorf("дублирующийся id=%q в документе", m[1])
		}
		seen[m[1]] = true
	}
}

// TestWebHostGroupThresholdLegacyEditLink — старый формат ссылки
// «Редактировать» (?gt_scope=&gt_label=, закладки и переходы из писем)
// продолжает работать: сервер открывает модалку правки найденного правила;
// несуществующая пара — обычная страница без открытых модалок, без 404.
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

	// Пара существует → модалка правки открыта с сервера.
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

	// Пары нет (правило могли удалить) → 200 и ни одной открытой модалки.
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

// TestWebHostGroupThresholdScopeLabelValidation — hostGroupThresholdSave,
// две ветки проверки scope/label, которые TestWebHostGroupThresholdsFlow не
// бьёт (noScopeForm там — пустой scope И пустой label одновременно): валидный
// scope с ПУСТЫМ label (ключ UNIQUE(project_id, scope, ”) собрал бы
// несвязанные правила в одну строку, см. докблок hostGroupThresholdSave) и
// label длиннее maxGroupThresholdLabelLen (256 рун, it-sec P2-1 ремедиации,
// B2) — обе 422 с тем же сообщением error.hostsettings.group_scope_label,
// правило не создаётся. Плюс: удаление чужим (не оператором) → 404, как у
// сохранения (requireProjectOperator, тот же гейт).
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

	// Валидный scope, пустой label → 422, правило не создано.
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

	// label длиннее 256 рун → 422, правило не создано (it-sec P2-1: без
	// границы GroupThresholdService.List читал бы её заново на каждом тике
	// оценщика).
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

	// label РОВНО на границе (256 рун) — валиден, правило создаётся.
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

	// Удаление чужим (не член организации, не оператор) → 404, правило не
	// удалено — тот же гейт requireProjectOperator, что у save.
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

// TestWebHostsListStatusSurvivesManyClosedIncidents — ревью I3: список хостов
// сворачивал открытые виды из «последних N инцидентов проекта ЛЮБОГО статуса»
// с лимитом 500. В проекте, где закрытых инцидентов накопилось больше лимита,
// открытый в выборку не попадал вовсе — хост с живой проблемой показывался
// спокойным. Здесь закрытых заведомо больше лимита и все они СВЕЖЕЕ открытого.
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

	// Открытый инцидент — САМЫЙ СТАРЫЙ из всех.
	open, _, err := s.incidents.Open(ctx, project.ID, hst.ID, "disk", 0.99, "", false)
	if err != nil {
		t.Fatalf("open disk incident: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		"UPDATE host_incidents SET started_at = now() - interval '1 day' WHERE id = $1", open.ID); err != nil {
		t.Fatalf("состарить открытый инцидент: %v", err)
	}
	// 600 закрытых инцидентов свежее открытого — больше прежнего лимита в 500.
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

// TestWebHostSettingsSaveResolvesDisabledKindIncidents — ревью I2: выключение
// порога должно иметь обратную силу.
//
// Оценщик выключенный вид пропускает целиком, ручного закрытия инцидента хоста
// в интерфейсе нет — до правки оператор, выключивший шумный порог, оставался с
// вечно красным бейджем «Диск» на списке хостов и не мог его снять ничем.
// Проверяем оба направления: инцидент выключенного вида закрыт, инцидент
// оставшегося включённым — нет.
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

	// Список хостов до правки настроек — хост «проблемный».
	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/hosts"
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "badge-danger") {
		t.Fatalf("до выключения порога на списке нет проблемного бейджа: %s", body)
	}

	// Выключаем ТОЛЬКО диск, остальные пороги остаются включёнными.
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

	// Закрытый инцидент диска действительно закрыт, с моментом закрытия.
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

// TestWebHostsListEmptyStateOnboardingConfig — пустой список хостов с
// активным публичным ключом проекта показывает готовый конфиг коллектора
// (endpoint+Bearer) и кнопку копирования (copy.js контракт).
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
	// В HTML-выводе кавычки внутри textarea экранированы (&#34;) — это
	// корректный текстовый узел, браузер декодирует его обратно в "Bearer
	// <ключ>" при чтении value; сырую (неэкранированную) строку конфига
	// проверяет TestCollectorConfig (hosts_test.go), здесь важно, что сам
	// ключ проекта попал в блок.
	if !strings.Contains(text, "Bearer "+key.PublicKey) {
		t.Errorf("нет Bearer-заголовка с публичным ключом проекта: %s", text)
	}
	if !strings.Contains(text, `data-copy-format="txt"`) {
		t.Errorf("нет кнопки копирования конфига (copy.js контракт): %s", text)
	}
	// Видимый <pre> рядом с кнопкой (UX-аудит A1, P1-1): скрытая textarea
	// aria-hidden, то есть до него онбординг был слеп и для скринридера, и
	// для глаза — проверить подставленные endpoint/ключ было нечем.
	if !strings.Contains(text, `<pre class="copy-preview">`) {
		t.Errorf("конфиг коллектора не отрисован видимым блоком: %s", text)
	}
}

// TestWebHostDetail — GET /projects/{id}/hosts/{name}: 200 с маркерами всех
// семи графиков (§5.3) и блоком открытых инцидентов; имя хоста с пробелом и
// кириллицей (URL-escaped) разбирается корректно; несуществующий хост и
// хост чужого проекта (не член организации) → 404.
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

	// Имя с пробелом и кириллицей — {name} должно URL-экранироваться в ссылке
	// (hostDetailPath) и корректно разбираться r.PathValue обратно.
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
	// Открытый инцидент диска — виден в блоке открытых инцидентов.
	if !strings.Contains(text, "Диск") {
		t.Errorf("нет блока открытых инцидентов (вид «Диск»): %s", text)
	}
	// P1-3: значение инцидента печатается юнитом ВИДА порога (host.ValueLabel),
	// а не сырым числом: диск 0.95 — это «95.0%», как и в списке хостов.
	if !strings.Contains(text, "95.0%") {
		t.Errorf("значение инцидента диска не в процентах: %s", text)
	}
	if strings.Contains(text, ">0.95<") {
		t.Errorf("значение инцидента осталось сырой долей: %s", text)
	}
	// P2-1: у хоста БЕЗ истории инцидентов пустое состояние — подсказка
	// строкой, как у блока открытых инцидентов, а не emptyState: его <h3>
	// печатался тем же ключом, что <h2> секции, и заголовок «Последние
	// инциденты» шёл дважды подряд. (У хоста выше история непуста, и второе
	// вхождение там законно — это aria-label скролл-области таблицы.)
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

	// Несуществующее имя хоста в существующем проекте → 404.
	missing := base + "/no-such-host"
	resp = getWithCookie(t, s.srv, missing, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (missing host) status = %d, want 404", missing, resp.StatusCode)
	}

	// Чужой (не член организации) → 404 (существование хоста не палится).
	_, outsider := orgSettingsRegister(t, s.auth, "hostdetail-outsider@example.com")
	resp = getWithCookie(t, s.srv, path, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider GET %s status = %d, want 404", path, resp.StatusCode)
	}
}

// TestWebHostDetailLogsLink — C3 «логи в контексте»: карточка хоста несёт
// ссылку на /logs с атрибут-фильтром res:host.name:<имя> (использует тот же
// logsForHostPath, что и раздел трейсов, Task 3), url-экранированную (":" →
// "%3A" через url.Values.Encode).
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

// TestWebHostDetailNilDeps — Metrics/Hosts проставлены, а HostIncidents или
// HostSettings — нет (тот же инвариант-нарушающий стенд, что и в
// TestWebHostsListNilHostsStore, T14 находка 1): hostDetail тоже должен
// звать h.notFound, а не паниковать на nil-указателе.
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

// TestWebHostThresholdsSaveFlow — POST /projects/{id}/hosts/{name}/thresholds
// (B2, T6): без Origin → 403 и override не меняется; чужой (не оператор) →
// 404 (requireProjectOperator); валидный POST по трём режимам (override/off/
// inherit) → 303 на карточку + flash "сохранено" + override в PG совпадает с
// формой, а GET-карточка после сохранения показывает эффективные
// значения/источники (host для переопределённого, "выключено" для off);
// невалидный POST (значение вне границы) → 422 с сообщением И введённым
// значением в форме, ранее сохранённый override НЕ подменяется мусором.
// assertThresholdGrid — сетка .threshold-grid присутствует в форме порогов и
// оборачивает ровно четыре карточки-fieldset (Диск/Память/Нагрузка/Тишина):
// открытие сетки стоит до первого fieldset, все четыре закрываются до кнопки
// «Сохранить», и после последнего из них закрывается сама обёртка. Общий на
// обе формы (host-settings-form и host-thresholds-form) — разметка одинаковая.
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

	// GET без override — форма оператора, все режимы "inherit" (нет
	// сохранённой строки override), эффективные значения — дефолт проекта
	// (проектных настроек тоже ещё нет — host.DefaultSettings, LevelDefault).
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

	// Без Origin → 403, override не сохраняется.
	resp = postForm(t, s.srv, savePath, validForm, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}
	if got, err := s.overrides.Get(ctx, hst.ID); err != nil || got.DiskEnabled != nil {
		t.Fatalf("override изменился без Origin: %+v, err=%v", got, err)
	}

	// Чужой (не член организации, не оператор) → 404, override не меняется.
	_, outsider := orgSettingsRegister(t, s.auth, "hthr-outsider@example.com")
	resp = postForm(t, s.srv, savePath, validForm, s.srv.URL, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider POST status = %d, want 404", resp.StatusCode)
	}

	// Валидный POST → 303 на карточку, flash "сохранено", override в PG.
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

	// GET после сохранения — эффективные значения отражают override:
	// disk 50% (источник — этот хост), memory «выключено».
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

	// Невалидный POST (диск вне границы 1..100%) → 422, сообщение + введённое
	// значение в форме, ранее сохранённый override НЕ подменяется мусором.
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

	// Несуществующее имя хоста → 404.
	resp = postForm(t, s.srv, "/projects/"+strconv.FormatInt(project.ID, 10)+"/hosts/no-such-host/thresholds", validForm, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing host POST status = %d, want 404", resp.StatusCode)
	}
}

// TestWebHostThresholdsSaveInvalidMemoryLoadSilent — hostThresholdsSave, три
// ветки hostSettingsErrorMessage/errors.Is, которые
// TestWebHostThresholdsSaveFlow не бьёт (там невалиден только disk):
// значения вне границ памяти/нагрузки/тишины проходят parseHostThresholdsForm
// (числа сами по себе валидны — не NaN/Inf), но отвергаются
// HostOverrideService.Save → ValidateOverride (host/override.go) — тот же
// сентинел-набор host.ErrInvalid*, что и у диска, но другая ветка switch в
// hostThresholdsSave/hostSettingsErrorMessage. Silent — отдельный случай:
// 1 минута не переполняет parseHostThresholdsForm (граница там — 0..720
// минут, host.MaxSilentAfter), но меньше host.MinSilentAfter (3 минуты) —
// ошибка возникает именно на Save, не на разборе формы.
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

// TestWebHostDeleteConfirmFlow — POST /projects/{id}/hosts/{name}/delete:
// без Origin → 403 (denyCrossOrigin); чужой (не член организации) → 404
// (requireProjectOperator); без confirmed=yes → 200 страница подтверждения,
// хост НЕ удалён, HostForget.Forget не вызван; с confirmed=yes → 303 на
// список, хост удалён из PG, HostForget.Forget(projectID, name) вызван ровно
// один раз.
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

	// Без Origin → 403.
	resp := postForm(t, s.srv, deletePath, url.Values{"confirmed": {"yes"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-origin status = %d, want 403", resp.StatusCode)
	}
	if forgetter.callCount() != 0 {
		t.Fatalf("Forget called on no-origin request: %d", forgetter.callCount())
	}

	// Чужой (не член организации) → 404, хост жив, Forget не вызван.
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

	// БЕЗ confirmed=yes → 200 страница подтверждения, хост жив, Forget не вызван.
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

	// С confirmed=yes → 303 на список, хост удалён, Forget вызван один раз.
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

// TestWebHostDeleteNilHostForget — HostForget не проставлен (main.go не
// всегда его проводит — режимы без ingest, см. комментарий у
// web.HostForgetter): удаление обязано пройти без паники, просто не
// реактивируя троттлер.
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

// hasFlashCookie — стоит ли в ответе flash-cookie с ожидаемым «вид|ключ».
// Значение уходит url.QueryEscape'нутым (см. setFlash, flash.go), поэтому
// сравнивать надо после разэкранирования, а не по сырой строке заголовка.
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
