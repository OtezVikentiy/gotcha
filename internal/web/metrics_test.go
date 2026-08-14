package web_test

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
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

type metricsStack struct {
	pool *pgxpool.Pool
	ch   driver.Conn
	srv  *httptest.Server
	org  *org.Service
	auth *auth.Service
}

func newMetricsStack(t *testing.T, wireMetrics bool) *metricsStack {
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
	if wireMetrics {
		h.Metrics = metric.NewQuery(ch)
	}
	h.Register(mux)
	return &metricsStack{pool: pool, ch: ch, srv: srv, org: orgSvc, auth: authSvc}
}

func (s *metricsStack) seedGauge(t *testing.T, projectID int64, name, env string, val float64, attrs map[string]string) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	if err := s.ch.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'gauge', '1', 'api', ?, ?, ?, ?, 0, [], [], 0, '')`,
		projectID, name, env, attrs, time.Now().UTC().Add(-time.Minute), val); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}
}

func TestWebMetricsList(t *testing.T) {
	s := newMetricsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "metrics-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "m-co", "M Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "m-proj", "M Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	s.seedGauge(t, project.ID, "cpu.usage", "prod", 0.5, map[string]string{"host": "h1"})

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/metrics"

	// Список метрик содержит имя.
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", base, resp.StatusCode)
	}
	if !strings.Contains(string(body), "cpu.usage") {
		t.Fatalf("list missing metric: %s", body)
	}

	// Страница метрики: график (SVG) и селекторы.
	detail := base + "/cpu.usage?period=24h&agg=avg"
	resp = getWithCookie(t, s.srv, detail, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", detail, resp.StatusCode)
	}
	if !strings.Contains(string(body), "<svg") {
		t.Fatalf("detail missing chart svg: %s", body)
	}

	// Несуществующая метрика → 404.
	resp = getWithCookie(t, s.srv, base+"/nope", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown metric status = %d, want 404", resp.StatusCode)
	}

	// Чужой → 404.
	_, outsider := orgSettingsRegister(t, s.auth, "metrics-outsider@example.com")
	resp = getWithCookie(t, s.srv, base, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}
}

// TestWebMetricsListSystemFilter — метрики хостового коллектора (system.*)
// затопили бы список метрик проекта после подключения хоста (T14–T16):
// по умолчанию они скрыты за переключателем со счётчиком, ?system=1
// показывает всё (§5.6 дизайна).
func TestWebMetricsListSystemFilter(t *testing.T) {
	s := newMetricsStack(t, true)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "metrics-sys-owner@example.com")
	o, err := s.org.CreateOrg(ctx, "ms-co", "MS Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, "ms-proj", "MS Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	s.seedGauge(t, project.ID, "app.requests", "prod", 42, nil)
	s.seedGauge(t, project.ID, "system.cpu.utilization", "prod", 0.3, nil)

	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/metrics"

	// По умолчанию: видна прикладная метрика, системная скрыта, есть
	// переключатель со счётчиком скрытых (1).
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", base, resp.StatusCode)
	}
	if !strings.Contains(string(body), "app.requests") {
		t.Fatalf("list missing app metric: %s", body)
	}
	if strings.Contains(string(body), "system.cpu.utilization") {
		t.Fatalf("system metric should be hidden by default: %s", body)
	}
	if !strings.Contains(string(body), "1") {
		t.Fatalf("hidden count (1) not shown: %s", body)
	}

	// ?system=1: обе метрики видны.
	resp = getWithCookie(t, s.srv, base+"?system=1", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?system=1 status = %d, want 200", base, resp.StatusCode)
	}
	if !strings.Contains(string(body), "app.requests") {
		t.Fatalf("list missing app metric with system=1: %s", body)
	}
	if !strings.Contains(string(body), "system.cpu.utilization") {
		t.Fatalf("system metric not shown with system=1: %s", body)
	}
}

func TestWebMetricsNilService(t *testing.T) {
	s := newMetricsStack(t, false)
	ctx := context.Background()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "metrics-nil-owner@example.com")
	o, _ := s.org.CreateOrg(ctx, "mn-co", "MN Co", ownerID)
	project, _ := s.org.CreateProject(ctx, o.ID, "mn-proj", "MN Proj", "go")
	base := "/projects/" + strconv.FormatInt(project.ID, 10) + "/metrics"
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil Metrics status = %d, want 404", resp.StatusCode)
	}
}
