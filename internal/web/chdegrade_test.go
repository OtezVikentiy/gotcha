package web_test

// Деградация страниц при отказе ClickHouse (аудит 2026-09-04, K8-1): на один
// и тот же отказ CH логи деградировали молча, а остальные CH-страницы отдавали
// 500 на весь экран — вместе с навигацией. Единый приём теперь один: оболочка
// живая, на месте блока данных — «данные временно недоступны», ошибка в лог.
//
// Шов — testenv.BrokenCH: реальный драйвер на закрытый локальный порт, любой
// запрос падает мгновенно. Продуктовые типы (metric.Query, trace.Query и
// прочие) конкретные, подменять их нечем — и не нужно: отказ сети и есть
// сценарий инцидента у пользователя.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// railMarker — основная навигация layout: её присутствие в теле и есть
// «оболочка жива».
const railMarker = `<nav class="rail"`

// emptyStateMarker — единый пустой экран (templates/emptystate.templ).
const emptyStateMarker = `class="empty-state"`

type brokenCHStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	uptime *uptime.Service
	hosts  *host.Store
	issues *issue.Service
}

// newBrokenCHStack собирает web.Handler со ВСЕМИ CH-читателями на битом
// соединении; PostgreSQL — настоящий (списки хостов/мониторов и доступ).
func newBrokenCHStack(t *testing.T) *brokenCHStack {
	t.Helper()
	return newBrokenCHStackWith(t, testenv.BrokenCH(t))
}

// newBrokenCHStackWith — то же, но с переданным битым соединением (например,
// считающим попытки — testenv.BrokenCHCounting).
func newBrokenCHStackWith(t *testing.T, ch driver.Conn) *brokenCHStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	issueSvc := issue.NewService(pool)
	h := web.New(authSvc, orgSvc, issueSvc, event.NewQuery(ch), srv.URL)
	h.Metrics = metric.NewQuery(ch)
	h.Trace = trace.NewQuery(ch)
	h.Profiles = profile.NewQuery(ch)
	h.UptimeQuery = uptime.NewQuery(ch)
	uptimeSvc := uptime.NewService(pool)
	h.Uptime = uptimeSvc
	hostsStore := host.NewStore(pool)
	h.Hosts = hostsStore
	h.HostIncidents = host.NewIncidentService(pool)
	h.HostSettings = host.NewSettingsService(pool)
	h.HostOverrides = host.NewHostOverrideService(pool)
	h.GroupThresholds = host.NewGroupThresholdService(pool)
	h.Register(mux)
	return &brokenCHStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, uptime: uptimeSvc, hosts: hostsStore, issues: issueSvc}
}

// getBody — GET с cookie, тело строкой.
func getBody(t *testing.T, srv *httptest.Server, path string, cookie *http.Cookie) (int, string) {
	t.Helper()
	resp := getWithCookie(t, srv, path, cookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

// TestWebCHDownPagesDegrade: каждая CH-страница при отказе хранилища —
// 200, оболочка с навигацией на месте, в блоке данных — текст ошибки
// (не «нет данных», не 500). Таблица — по одной строке на страницу.
func TestWebCHDownPagesDegrade(t *testing.T) {
	s := newBrokenCHStack(t)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "chdown-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "chdown-co", "CH Down Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "chdown-proj", "CH Down Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-1"}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	mon := baseMonitor(project.ID, "site-main")
	mon.Config = monHTTPConfig(t, s.srv.URL+"/healthz")
	created, err := s.uptime.Create(ctx, mon, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	iss, err := s.issues.Upsert(ctx, project.ID, "fp-chdown", "NullPointerException", "pkg/a.go:10", "error", "", time.Now())
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}
	issuePath := "/issues/" + strconv.FormatInt(iss.IssueID, 10)
	monitorPath := "/monitors/" + strconv.FormatInt(created.ID, 10)

	base := "/projects/" + strconv.FormatInt(project.ID, 10)
	cases := []struct {
		name string
		path string
		// errText — заголовок/подсказка состояния «недоступно» (ru — локаль
		// по умолчанию). Именно текст ошибки, а не пустого состояния: иначе
		// тест не отличил бы деградацию от «данных пока нет».
		errText string
		// emptyState — блок данных заменён единым пустым экраном; списки
		// PG-сущностей (хосты, мониторы) вместо него показывают подсказку
		// над таблицей, сама таблица остаётся.
		emptyState bool
		// keep — PG-данные, которые обязаны остаться на странице.
		keep string
	}{
		{name: "metrics list", path: base + "/metrics", errText: "Не удалось загрузить метрики", emptyState: true},
		{name: "metric detail", path: base + "/metrics/cpu.usage", errText: "Не удалось загрузить метрики", emptyState: true, keep: "cpu.usage"},
		{name: "trace waterfall", path: "/traces/0123456789abcdef", errText: "Не удалось загрузить трейс", emptyState: true, keep: "0123456789abcdef"},
		{name: "trace flame", path: "/traces/0123456789abcdef/flame", errText: "Не удалось загрузить трейс", emptyState: true},
		{name: "hosts list", path: base + "/hosts", errText: "Метрики временно недоступны", keep: "web-1"},
		{name: "host detail", path: base + "/hosts/web-1", errText: "Не удалось загрузить графики", emptyState: true, keep: "web-1"},
		{name: "monitors list", path: base + "/monitors", errText: "Аптайм и задержки временно недоступны", keep: "site-main"},
		{name: "performance list", path: base + "/performance", errText: "Не удалось загрузить транзакции", emptyState: true},
		{name: "endpoint detail", path: base + "/performance/GET%20%2Fapi", errText: "Не удалось загрузить транзакции", emptyState: true, keep: "GET /api"},
		{name: "profiles list", path: base + "/profiles", errText: "Не удалось загрузить профили", emptyState: true},
		{name: "profile flame", path: base + "/profiles/flame?service=web&type=cpu", errText: "Не удалось загрузить профили", emptyState: true},
		{name: "web vitals list", path: base + "/web-vitals", errText: "Не удалось загрузить Web Vitals", emptyState: true},
		{name: "overview status line", path: base + "/overview", errText: ">недоступно<", keep: "Аптайм за окно"},
		{name: "issues list", path: base + "/issues", errText: "Графики частоты временно недоступны", keep: "NullPointerException"},
		{name: "issue detail", path: issuePath, errText: "Не удалось загрузить события", emptyState: true, keep: "NullPointerException"},
		{name: "monitor detail", path: monitorPath, errText: "Не удалось загрузить проверки", emptyState: true, keep: "site-main"},
		{name: "monitor detail uptime tiles", path: monitorPath, errText: ">недоступно<", keep: "site-main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := getBody(t, s.srv, tc.path, ownerCookie)
			if status != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200 (degraded page, not an error screen)", tc.path, status)
			}
			if !strings.Contains(body, railMarker) {
				t.Fatalf("GET %s: navigation rail missing — page shell not preserved", tc.path)
			}
			if !strings.Contains(body, tc.errText) {
				t.Fatalf("GET %s: unavailable-state text %q missing in body", tc.path, tc.errText)
			}
			if tc.emptyState && !strings.Contains(body, emptyStateMarker) {
				t.Fatalf("GET %s: empty-state block missing", tc.path)
			}
			if tc.keep != "" && !strings.Contains(body, tc.keep) {
				t.Fatalf("GET %s: PostgreSQL-backed content %q missing — degraded too far", tc.path, tc.keep)
			}
		})
	}
}

// TestWebCHDownFirstFailureStopsPolling: страницы, делающие несколько
// запросов к ClickHouse (список хостов — 5×LatestByHost, список мониторов —
// 3 батча), после первого отказа в хранилище больше не ходят: на странице
// ровно одна попытка подключения, а не по одной на запрос. Счётчик —
// testenv.BrokenCHCounting (одна попытка на запрос, см. testenv-тест).
func TestWebCHDownFirstFailureStopsPolling(t *testing.T) {
	ch, attempts := testenv.BrokenCHCounting(t)
	s := newBrokenCHStackWith(t, ch)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "chdown-poll@example.com")
	o, err := s.org.CreateOrg(ctx, "chdown-poll", "CH Down Poll", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "chdown-poll-proj", "P", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.hosts.Upsert(ctx, project.ID, []host.TouchEntry{{Name: "web-1"}}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	mon := baseMonitor(project.ID, "site-main")
	mon.Config = monHTTPConfig(t, s.srv.URL+"/healthz")
	if _, err := s.uptime.Create(ctx, mon, []string{"local"}, nil); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	base := "/projects/" + strconv.FormatInt(project.ID, 10)
	for _, tc := range []struct {
		name, path, errText string
	}{
		{"hosts list", base + "/hosts", "Метрики временно недоступны"},
		{"monitors list", base + "/monitors", "Аптайм и задержки временно недоступны"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts.Store(0)
			status, body := getBody(t, s.srv, tc.path, ownerCookie)
			if status != http.StatusOK || !strings.Contains(body, tc.errText) {
				t.Fatalf("GET %s status = %d, degraded text present = %v", tc.path, status, strings.Contains(body, tc.errText))
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("GET %s: %d ClickHouse connection attempts, want exactly 1 — polling must stop after the first failure", tc.path, got)
			}
		})
	}
}

// TestWebCHDownOverviewWithoutMonitors: без мониторов обзор к CH не ходит
// и плитка аптайма показывает обычное «нет данных», а не «недоступно» —
// флаг ставится только по реальному отказу запроса.
func TestWebCHDownOverviewWithoutMonitors(t *testing.T) {
	s := newBrokenCHStack(t)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "chdown-nomon@example.com")
	o, err := s.org.CreateOrg(ctx, "chdown-nomon", "CH Down NoMon", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "chdown-nomon-proj", "P", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(project.ID, 10) + "/overview"
	status, body := getBody(t, s.srv, path, ownerCookie)
	if status != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", path, status)
	}
	if strings.Contains(body, ">недоступно<") {
		t.Fatalf("GET %s: uptime tile says unavailable without any monitor (no CH query was made)", path)
	}
}

// TestWebTraceNotFoundStays404: «трейса нет» — по-прежнему 404 (ProjectForTrace
// вернул found=false БЕЗ ошибки), деградация в 200 — только при отказе CH.
// Здесь ClickHouse настоящий и пустой.
func TestWebTraceNotFoundStays404(t *testing.T) {
	s := newTraceStack(t)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "trace-404@example.com")
	if _, err := s.org.CreateOrg(ctx, "trace-404", "Trace 404", ownerID); err != nil {
		t.Fatalf("create org: %v", err)
	}
	for _, path := range []string{"/traces/0123456789abcdef", "/traces/0123456789abcdef/flame"} {
		status, body := getBody(t, s.srv, path, ownerCookie)
		if status != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404 for unknown trace", path, status)
		}
		if strings.Contains(body, "Не удалось загрузить трейс") {
			t.Fatalf("GET %s: unknown trace rendered as storage outage", path)
		}
	}
}
