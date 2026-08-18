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
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// logsStack — мигрированные PG+CH и Handler для /projects/{id}/logs, как
// newHostsStack (hosts_web_test.go). wireLogQuery=false моделирует стенд без
// проводки логов (h.LogQuery остаётся nil) — для проверки гейта в logsList.
type logsStack struct {
	pool *pgxpool.Pool
	ch   driver.Conn
	srv  *httptest.Server
	h    *web.Handler
	org  *org.Service
	auth *auth.Service
}

func newLogsStack(t *testing.T, wireLogQuery bool) *logsStack {
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
	if wireLogQuery {
		h.LogQuery = log.NewQuery(ch)
	}
	h.Register(mux)
	return &logsStack{pool: pool, ch: ch, srv: srv, h: h, org: orgSvc, auth: authSvc}
}

// seedLogs пишет записи через log.Writer и синхронно доливает буфер (Close),
// как internal/log/query_test.go — детерминированно для GET сразу после.
func (s *logsStack) seedLogs(t *testing.T, projectID int64, records ...log.LogRecord) {
	t.Helper()
	w := log.NewWriter(s.ch)
	go w.Run()
	for _, r := range records {
		w.Add(projectID, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed logs: %v", err)
	}
}

func newLogsProject(t *testing.T, s *logsStack, ownerEmail, orgSlug, projSlug string) (int64, *http.Cookie, org.Project) {
	t.Helper()
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, ownerEmail)
	ctx := context.Background()
	o, err := s.org.CreateOrg(ctx, orgSlug, orgSlug, ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(ctx, o.ID, projSlug, projSlug, "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return ownerID, ownerCookie, project
}

func logsBasePath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/logs"
}

// TestWebLogsList покрывает основной сценарий Step 1 брифа: список виден
// (тело/severity/service), фильтры severity/q сужают и сохраняются в форме,
// чужой проект — 404, неавторизованный — редирект на /login.
func TestWebLogsList(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-owner@example.com", "logs-co", "logs-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{
			Timestamp: now.Add(-time.Minute), ObservedTS: now.Add(-time.Minute),
			Severity: log.SevInfo, SeverityNumber: 9, SeverityText: "INFO",
			Body: "request handled ok", Service: "api", Environment: "production",
		},
		log.LogRecord{
			Timestamp: now.Add(-2 * time.Minute), ObservedTS: now.Add(-2 * time.Minute),
			Severity: log.SevError, SeverityNumber: 17, SeverityText: "ERROR",
			Body: "boom happened", Service: "worker", Environment: "staging",
		},
	)

	base := logsBasePath(project.ID)
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)
	for _, want := range []string{"request handled ok", "boom happened", "api", "worker"} {
		if !strings.Contains(text, want) {
			t.Errorf("список не содержит %q: %s", want, text)
		}
	}

	// ?severity=error сужает список и остаётся отмеченным в форме.
	resp = getWithCookie(t, s.srv, base+"?severity=error", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?severity=error status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text = string(body)
	if strings.Contains(text, "request handled ok") {
		t.Errorf("severity=error не отфильтровал info-запись: %s", text)
	}
	if !strings.Contains(text, "boom happened") {
		t.Errorf("severity=error потерял error-запись: %s", text)
	}
	if !strings.Contains(text, `value="error" checked`) {
		t.Errorf("чекбокс severity=error не отмечен в форме: %s", text)
	}
	if strings.Contains(text, `value="info" checked`) {
		t.Errorf("чекбокс severity=info отмечен, хотя не выбран: %s", text)
	}

	// ?q=boom фильтрует по телу.
	resp = getWithCookie(t, s.srv, base+"?q=boom", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if strings.Contains(text, "request handled ok") {
		t.Errorf("q=boom не отфильтровал непохожую запись: %s", text)
	}
	if !strings.Contains(text, "boom happened") {
		t.Errorf("q=boom потерял подходящую запись: %s", text)
	}

	// Чужой (не член организации) → 404.
	_, outsider := orgSettingsRegister(t, s.auth, "logs-outsider@example.com")
	resp = getWithCookie(t, s.srv, base, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}

	// Неавторизованный → редирект на /login.
	resp = getWithCookie(t, s.srv, base, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("unauthenticated redirect Location = %q, want prefix /login", loc)
	}
}

// TestWebLogsListEmptyStates — оба пустых состояния: проект без единого лога
// («logs.empty.none») и проект с логами, но фильтр ничего не оставил
// («logs.empty.filter»).
func TestWebLogsListEmptyStates(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-empty-owner@example.com", "logs-empty-co", "logs-empty-proj")

	base := logsBasePath(project.ID)
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "No logs yet") && !strings.Contains(string(body), "Логов пока нет") {
		t.Errorf("нет пустого состояния «логов пока нет»: %s", body)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID, log.LogRecord{
		Timestamp: now, ObservedTS: now, Severity: log.SevInfo, Body: "only info log", Service: "api",
	})

	resp = getWithCookie(t, s.srv, base+"?severity=fatal", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?severity=fatal status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)
	if strings.Contains(text, "only info log") {
		t.Errorf("severity=fatal не отфильтровал info-запись: %s", text)
	}
	if !strings.Contains(text, "Nothing matches the filters") && !strings.Contains(text, "Ничего не подошло под фильтры") {
		t.Errorf("нет пустого состояния «ничего не подошло под фильтры»: %s", text)
	}
}

// TestWebLogsListCursorPagination — ?before=<unix_ms>&tskip=<n> отрезает
// более новые строки и пропускает уже показанные строки тай-группы (та же
// семантика, что и log.Query.List, проверенная в internal/log/query_test.go;
// здесь — что параметры действительно долетают от query до log.ListFilter).
func TestWebLogsListCursorPagination(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-cursor-owner@example.com", "logs-cursor-co", "logs-cursor-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	t1 := now.Add(-3 * time.Minute)
	t2 := now.Add(-2 * time.Minute)
	t3 := now.Add(-1 * time.Minute)
	s.seedLogs(t, project.ID,
		log.LogRecord{Timestamp: t1, ObservedTS: t1, Severity: log.SevInfo, Body: "log one"},
		log.LogRecord{Timestamp: t2, ObservedTS: t2, Severity: log.SevInfo, Body: "log two"},
		log.LogRecord{Timestamp: t3, ObservedTS: t3, Severity: log.SevInfo, Body: "log three"},
	)

	base := logsBasePath(project.ID)

	// Без курсора — видны все три.
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(body)
	for _, want := range []string{"log one", "log two", "log three"} {
		if !strings.Contains(text, want) {
			t.Fatalf("без курсора не видно %q: %s", want, text)
		}
	}

	before := strconv.FormatInt(t2.UnixMilli(), 10)

	// before=t2, tskip=0 — отрезает t3 (новее), t2 и t1 остаются.
	resp = getWithCookie(t, s.srv, base+"?before="+before, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if strings.Contains(text, "log three") {
		t.Errorf("before=t2 не отрезал более новую строку: %s", text)
	}
	if !strings.Contains(text, "log two") || !strings.Contains(text, "log one") {
		t.Errorf("before=t2 потерял строки на границе или старее: %s", text)
	}

	// before=t2, tskip=1 — пропускает саму строку t2 (уже показана на
	// предыдущей «странице»), остаётся только t1.
	resp = getWithCookie(t, s.srv, base+"?before="+before+"&tskip=1", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if strings.Contains(text, "log three") || strings.Contains(text, "log two") {
		t.Errorf("before=t2&tskip=1 не пропустил уже показанные строки: %s", text)
	}
	if !strings.Contains(text, "log one") {
		t.Errorf("before=t2&tskip=1 потерял самую старую строку: %s", text)
	}
}

// TestWebLogsListNilLogQuery404 — h.LogQuery == nil (стенд без проводки
// логов) отдаёт 404, а не паникует на разыменовании.
func TestWebLogsListNilLogQuery404(t *testing.T) {
	s := newLogsStack(t, false)
	_, ownerCookie, project := newLogsProject(t, s, "logs-noquery-owner@example.com", "logs-noquery-co", "logs-noquery-proj")

	resp := getWithCookie(t, s.srv, logsBasePath(project.ID), ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("h.LogQuery=nil status = %d, want 404", resp.StatusCode)
	}
}
