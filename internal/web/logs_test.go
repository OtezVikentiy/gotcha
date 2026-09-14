package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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

	_, outsider := orgSettingsRegister(t, s.auth, "logs-outsider@example.com")
	resp = getWithCookie(t, s.srv, base, outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}

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

func TestWebLogsListTraceLink(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-tracelink-owner@example.com", "logs-tracelink-co", "logs-tracelink-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{
			Timestamp: now.Add(-time.Minute), ObservedTS: now.Add(-time.Minute),
			Severity: log.SevInfo, Body: "row-with-trace", Service: "api",
			TraceID:       "trace-abc-123",
			LogAttributes: map[string]string{"http.method": "GET"},
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
	if !strings.Contains(text, `href="/traces/trace-abc-123"`) {
		t.Errorf("trace_id должен вести на /traces/trace-abc-123: %s", text)
	}
	i := strings.Index(text, `class="log-row-trace"`)
	if i == -1 {
		t.Fatalf("не нашли блок log-row-trace: %s", text)
	}
	traceBlock := text[i : i+300]
	if strings.Contains(traceBlock, "/performance") {
		t.Errorf("trace_id не должен вести на общий раздел «Производительность» без пометки: %s", traceBlock)
	}
	if !strings.Contains(text, `<h4 class="ctx-title">`+html.EscapeString("Атрибуты")+`</h4>`) {
		t.Errorf("таблица атрибутов раскрытой строки должна получить заголовок «Атрибуты»: %s", text)
	}
}

func TestWebLogsListRangeClamped(t *testing.T) {
	s := newLogsStack(t, true)
	s.h.LogRetentionDays = 3
	_, ownerCookie, project := newLogsProject(t, s, "logs-clamp-owner@example.com", "logs-clamp-co", "logs-clamp-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{Timestamp: now.Add(-time.Minute), ObservedTS: now.Add(-time.Minute), Severity: log.SevInfo, Body: "row-recent", Service: "api"},
	)

	base := logsBasePath(project.ID)

	resp := getWithCookie(t, s.srv, base+"?period=30d", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?period=30d status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)
	if !strings.Contains(text, "logs-range-clamped") {
		t.Errorf("period=30d при retentionDays=3 должен показать подпись о клампе: %s", text)
	}
	if !strings.Contains(text, "3 дн.") {
		t.Errorf("подпись о клампе должна упомянуть retentionDays=3 (i18n-подстановка {days}): %s", text)
	}

	resp = getWithCookie(t, s.srv, base+"?period=1h", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if strings.Contains(text, "logs-range-clamped") {
		t.Errorf("period=1h короче retentionDays=3, подписи о клампе быть не должно: %s", text)
	}
}

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

var logRowsTBodyRe = regexp.MustCompile(`(?s)<tbody>(.*?)</tbody>`)
var logRowBodyRe = regexp.MustCompile(`<summary>([^<]*)</summary>`)

var olderHrefRe = regexp.MustCompile(`<nav class="pagination"[^>]*><a href="([^"]+)">`)

var beforeParamRe = regexp.MustCompile(`before=(\d+)`)

func logRowBodiesOnPage(t *testing.T, htmlBody string) []string {
	t.Helper()
	m := logRowsTBodyRe.FindStringSubmatch(htmlBody)
	if m == nil {
		return nil
	}
	var out []string
	for _, sm := range logRowBodyRe.FindAllStringSubmatch(m[1], -1) {
		out = append(out, sm[1])
	}
	return out
}

func TestWebLogsListCursorNoDupNoLoss(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-walk-owner@example.com", "logs-walk-co", "logs-walk-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)

	const distinctCount = 5
	const tieCount = 250 // >> logsListLimit(100) — гарантированно многостраничный тай

	want := map[string]bool{}
	var records []log.LogRecord

	for i := 0; i < distinctCount; i++ {
		ts := now.Add(-time.Duration(i) * time.Second)
		body := "row-" + strconv.Itoa(i)
		records = append(records, log.LogRecord{Timestamp: ts, ObservedTS: ts, Severity: log.SevInfo, Body: body})
		want[body] = true
	}

	tieTS := now.Add(-time.Duration(distinctCount+1) * time.Second)
	for i := 0; i < tieCount; i++ {
		body := "tie-" + strconv.Itoa(i)
		records = append(records, log.LogRecord{Timestamp: tieTS, ObservedTS: tieTS, Severity: log.SevInfo, Body: body})
		want[body] = true
	}

	tailTS := tieTS.Add(-time.Minute)
	records = append(records, log.LogRecord{Timestamp: tailTS, ObservedTS: tailTS, Severity: log.SevInfo, Body: "tail"})
	want["tail"] = true

	s.seedLogs(t, project.ID, records...)

	path := logsBasePath(project.ID)
	seen := map[string]bool{}
	var lastBefore string
	sameBeforeStreak := 0
	pages := 0
	for {
		pages++
		if pages > 20 {
			t.Fatalf("слишком много страниц (%d) — вероятно, зацикливание курсора", pages)
		}
		resp := getWithCookie(t, s.srv, path, ownerCookie)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
		}
		text := string(body)

		for _, b := range logRowBodiesOnPage(t, text) {
			if seen[b] {
				t.Fatalf("дубль строки %q на странице %d (path=%s)", b, pages, path)
			}
			seen[b] = true
		}

		m := olderHrefRe.FindStringSubmatch(text)
		if m == nil {
			break
		}
		href := html.UnescapeString(m[1]) // атрибут href экранирован (& -> &amp;)
		if bm := beforeParamRe.FindStringSubmatch(href); bm != nil {
			if bm[1] == lastBefore {
				sameBeforeStreak++
			}
			lastBefore = bm[1]
		}
		path = href
	}

	if len(seen) != len(want) {
		var missing, extra []string
		for b := range want {
			if !seen[b] {
				missing = append(missing, b)
			}
		}
		for b := range seen {
			if !want[b] {
				extra = append(extra, b)
			}
		}
		t.Fatalf("покрытие разошлось: показано %d, ожидалось %d; отсутствуют=%v лишние=%v", len(seen), len(want), missing, extra)
	}
	if pages < 2 {
		t.Fatalf("страниц = %d, want >= 2 (тай-группа в %d строк на лимите 100 обязана растянуться на несколько страниц)", pages, tieCount)
	}
	if sameBeforeStreak < 1 {
		t.Errorf("ни одна пара страниц подряд не использовала один и тот же Before — тест не прогнал накопление TieSkip через несколько хопов")
	}
}

var logFacetSectionRe = regexp.MustCompile(`(?s)<section class="card logs-facet">(.*?)</section>`)

var logFacetItemRe = regexp.MustCompile(`<a class="(logs-facet-value[^"]*)" href="([^"]+)"[^>]*>([^<]*)</a>\s*<span class="logs-facet-count">(\d+)</span>`)

type logFacetItem struct {
	Active bool
	Href   string
	Label  string
	Count  string
}

func logFacetItems(t *testing.T, htmlBody string, sectionIdx int) []logFacetItem {
	t.Helper()
	sections := logFacetSectionRe.FindAllStringSubmatch(htmlBody, -1)
	if len(sections) <= sectionIdx {
		t.Fatalf("секция фасета #%d не найдена (всего секций: %d)", sectionIdx, len(sections))
	}
	var out []logFacetItem
	for _, m := range logFacetItemRe.FindAllStringSubmatch(sections[sectionIdx][1], -1) {
		out = append(out, logFacetItem{
			Active: strings.Contains(m[1], "logs-facet-value-active"),
			Href:   html.UnescapeString(m[2]),
			Label:  m[3],
			Count:  m[4],
		})
	}
	return out
}

func findFacetItem(items []logFacetItem, label string) (logFacetItem, bool) {
	for _, it := range items {
		if it.Label == label {
			return it, true
		}
	}
	return logFacetItem{}, false
}

func findSeverityInfoItem(items []logFacetItem) (logFacetItem, bool) {
	if it, ok := findFacetItem(items, "Info"); ok {
		return it, true
	}
	return findFacetItem(items, "Инфо")
}

func TestWebLogsListFacets(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-facets-owner@example.com", "logs-facets-co", "logs-facets-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{Timestamp: now.Add(-1 * time.Minute), ObservedTS: now.Add(-1 * time.Minute), Severity: log.SevInfo, Body: "row-api-info-1", Service: "api", Environment: "production"},
		log.LogRecord{Timestamp: now.Add(-2 * time.Minute), ObservedTS: now.Add(-2 * time.Minute), Severity: log.SevInfo, Body: "row-api-info-2", Service: "api", Environment: "production"},
		log.LogRecord{Timestamp: now.Add(-3 * time.Minute), ObservedTS: now.Add(-3 * time.Minute), Severity: log.SevError, Body: "row-api-error", Service: "api", Environment: "production"},
		log.LogRecord{Timestamp: now.Add(-4 * time.Minute), ObservedTS: now.Add(-4 * time.Minute), Severity: log.SevDebug, Body: "row-worker-debug", Service: "worker", Environment: "staging"},
	)

	base := logsBasePath(project.ID)
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)

	sevItems := logFacetItems(t, text, 0)
	svcItems := logFacetItems(t, text, 1)
	envItems := logFacetItems(t, text, 2)

	if it, ok := findSeverityInfoItem(sevItems); !ok || it.Count != "2" {
		t.Fatalf("severity facet: info count != 2: %+v", sevItems)
	}
	svcAPI, ok := findFacetItem(svcItems, "api")
	if !ok || svcAPI.Count != "3" {
		t.Fatalf("service facet: api count != 3: %+v", svcItems)
	}
	svcWorker, ok := findFacetItem(svcItems, "worker")
	if !ok || svcWorker.Count != "1" {
		t.Fatalf("service facet: worker count != 1: %+v", svcItems)
	}
	if svcAPI.Active || svcWorker.Active {
		t.Fatalf("без фильтров ни одно значение service не должно быть активным: api=%v worker=%v", svcAPI.Active, svcWorker.Active)
	}
	envProd, ok := findFacetItem(envItems, "production")
	if !ok || envProd.Count != "3" {
		t.Fatalf("environment facet: production count != 3: %+v", envItems)
	}
	envStaging, ok := findFacetItem(envItems, "staging")
	if !ok || envStaging.Count != "1" {
		t.Fatalf("environment facet: staging count != 1: %+v", envItems)
	}

	resp = getWithCookie(t, s.srv, svcWorker.Href, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (facet click) status = %d, want 200: %s", svcWorker.Href, resp.StatusCode, body)
	}
	text = string(body)
	if strings.Contains(text, "row-api-info-1") || strings.Contains(text, "row-api-error") {
		t.Errorf("клик по фасету service=worker не сузил список: %s", text)
	}
	if !strings.Contains(text, "row-worker-debug") {
		t.Errorf("клик по фасету service=worker потерял свою же строку: %s", text)
	}
	svcItemsAfter := logFacetItems(t, text, 1)
	workerAfter, ok := findFacetItem(svcItemsAfter, "worker")
	if !ok || !workerAfter.Active {
		t.Fatalf("после клика значение worker должно быть отмечено активным: %+v", svcItemsAfter)
	}
	apiAfter, ok := findFacetItem(svcItemsAfter, "api")
	if ok && apiAfter.Active {
		t.Fatalf("после выбора worker значение api не должно быть активным: %+v", svcItemsAfter)
	}

	resp = getWithCookie(t, s.srv, base+"?severity=error", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	sevItemsFiltered := logFacetItems(t, text, 0)
	infoAfter, ok := findSeverityInfoItem(sevItemsFiltered)
	if !ok || infoAfter.Count != "2" {
		t.Fatalf("exclude-self: severity=error не должен занулять count info: %+v", sevItemsFiltered)
	}
}

func TestFacetValueHasExcludeLink(t *testing.T) {
	s := newLogsStack(t, true)
	projectID, cookie, _ := newLogsProject(t, s, "facet-exclude@example.com", "facet-exclude-org", "facet-exclude-proj")

	now := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	s.seedLogs(t, projectID,
		log.LogRecord{
			Timestamp: now, ObservedTS: now,
			Severity: log.SevInfo, SeverityNumber: 9, SeverityText: "INFO",
			Body: "tick", Service: "cron", Environment: "production",
		},
	)

	resp := getWithCookie(t, s.srv, logsBasePath(projectID), cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	sections := logFacetSectionRe.FindAllStringSubmatch(page, -1)
	if len(sections) < 3 {
		t.Fatalf("ожидалось минимум 3 секции встроенных фасетов (severity/service/environment): найдено %d", len(sections))
	}
	svcSection := sections[1][1]
	envSection := sections[2][1]

	if !strings.Contains(svcSection, "service_not=cron") {
		t.Fatalf("у значения фасета service «cron» нет ссылки исключения: %s", svcSection)
	}
	if !strings.Contains(svcSection, "logs-row-action--exclude") {
		t.Fatalf("ссылка исключения фасета service не оформлена как кнопка-иконка (logs-row-action--exclude): %s", svcSection)
	}
	if !strings.Contains(envSection, "environment_not=production") {
		t.Fatalf("у значения фасета environment «production» нет ссылки исключения: %s", envSection)
	}
	if strings.Contains(svcSection, "before=") || strings.Contains(svcSection, "tskip=") {
		t.Errorf("ссылка исключения фасета service тащит курсор пагинации: %s", svcSection)
	}
}

func TestWebLogsListAttrFacets(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-attrfacets-owner@example.com", "logs-attrfacets-co", "logs-attrfacets-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{
			Timestamp: now.Add(-1 * time.Minute), ObservedTS: now.Add(-1 * time.Minute),
			Severity: log.SevInfo, Body: "row-get-1", Service: "api",
			LogAttributes: map[string]string{"http.method": "GET"},
		},
		log.LogRecord{
			Timestamp: now.Add(-2 * time.Minute), ObservedTS: now.Add(-2 * time.Minute),
			Severity: log.SevInfo, Body: "row-get-2", Service: "api",
			LogAttributes: map[string]string{"http.method": "GET"},
		},
		log.LogRecord{
			Timestamp: now.Add(-3 * time.Minute), ObservedTS: now.Add(-3 * time.Minute),
			Severity: log.SevInfo, Body: "row-post", Service: "api",
			LogAttributes: map[string]string{"http.method": "POST"},
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

	attrItems := logFacetItems(t, text, 3)
	keyItem, ok := findFacetItem(attrItems, "http.method")
	if !ok || keyItem.Count != "3" {
		t.Fatalf("attr facet: http.method count != 3: %+v", attrItems)
	}
	if keyItem.Active {
		t.Fatalf("нераскрытый ключ не должен быть помечен активным: %+v", keyItem)
	}
	if _, ok := findFacetItem(attrItems, "GET"); ok {
		t.Fatalf("значения нераскрытого ключа не должны быть видны: %+v", attrItems)
	}

	resp = getWithCookie(t, s.srv, keyItem.Href, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (facet key click) status = %d, want 200: %s", keyItem.Href, resp.StatusCode, body)
	}
	text = string(body)
	expandedItems := logFacetItems(t, text, 3)
	keyExpanded, ok := findFacetItem(expandedItems, "http.method")
	if !ok || !keyExpanded.Active {
		t.Fatalf("раскрытый ключ должен быть отмечен активным (aria-current): %+v", expandedItems)
	}
	getValue, ok := findFacetItem(expandedItems, "GET")
	if !ok || getValue.Count != "2" {
		t.Fatalf("attr facet values: GET count != 2: %+v", expandedItems)
	}
	postValue, ok := findFacetItem(expandedItems, "POST")
	if !ok || postValue.Count != "1" {
		t.Fatalf("attr facet values: POST count != 1: %+v", expandedItems)
	}
	if getValue.Active || postValue.Active {
		t.Fatalf("без выбранного значения ни GET, ни POST не должны быть активны: get=%v post=%v", getValue.Active, postValue.Active)
	}

	resp = getWithCookie(t, s.srv, getValue.Href, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (facet value click) status = %d, want 200: %s", getValue.Href, resp.StatusCode, body)
	}
	text = string(body)
	if strings.Contains(text, "row-post") {
		t.Errorf("клик по значению GET не сузил список: %s", text)
	}
	if !strings.Contains(text, "row-get-1") || !strings.Contains(text, "row-get-2") {
		t.Errorf("клик по значению GET потерял свои же строки: %s", text)
	}
	afterClickItems := logFacetItems(t, text, 3)
	getAfter, ok := findFacetItem(afterClickItems, "GET")
	if !ok || !getAfter.Active {
		t.Fatalf("после клика значение GET должно быть активным: %+v", afterClickItems)
	}

	resp = getWithCookie(t, s.srv, getAfter.Href, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if !strings.Contains(text, "row-post") {
		t.Errorf("повторный клик по активному GET не снял фильтр: %s", text)
	}
}

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

type attrKeyJSON struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

func TestWebLogsAttrKeysAutocomplete(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-attrkeys-owner@example.com", "logs-attrkeys-co", "logs-attrkeys-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{
			Timestamp: now.Add(-time.Minute), ObservedTS: now.Add(-time.Minute),
			Severity: log.SevInfo, Body: "row-1", Service: "api",
			LogAttributes: map[string]string{"http.method": "GET", "http.status_code": "200"},
		},
		log.LogRecord{
			Timestamp: now.Add(-2 * time.Minute), ObservedTS: now.Add(-2 * time.Minute),
			Severity: log.SevInfo, Body: "row-2", Service: "api",
			LogAttributes: map[string]string{"db.statement": "SELECT 1"},
		},
	)

	base := logsBasePath(project.ID) + "/attr-keys"

	resp := getWithCookie(t, s.srv, base+"?q=http.", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?q=http. status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got []attrKeyJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", body, err)
	}
	byKey := map[string]int64{}
	for _, it := range got {
		byKey[it.Key] = it.Count
	}
	if byKey["http.method"] != 1 || byKey["http.status_code"] != 1 {
		t.Errorf("attr-keys q=http. = %+v, want http.method=1 и http.status_code=1", got)
	}
	if _, ok := byKey["db.statement"]; ok {
		t.Errorf("attr-keys q=http. не должен вернуть db.statement (не совпадает по префиксу): %+v", got)
	}

	resp = getWithCookie(t, s.srv, base, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	got = nil
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", body, err)
	}
	byKey = map[string]int64{}
	for _, it := range got {
		byKey[it.Key] = it.Count
	}
	if _, ok := byKey["db.statement"]; !ok {
		t.Errorf("attr-keys без q должен вернуть db.statement тоже: %+v", got)
	}

	_, outsider := orgSettingsRegister(t, s.auth, "logs-attrkeys-outsider@example.com")
	resp = getWithCookie(t, s.srv, base+"?q=http.", outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}

	resp = getWithCookie(t, s.srv, base+"?q=http.", nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("unauthenticated redirect Location = %q, want prefix /login", loc)
	}
}

func TestWebLogsAttrKeysAutocompleteWindow(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-attrkeys-window-owner@example.com", "logs-attrkeys-window-co", "logs-attrkeys-window-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{
			Timestamp: now.Add(-time.Minute), ObservedTS: now.Add(-time.Minute),
			Severity: log.SevInfo, Body: "row-recent", Service: "api",
			LogAttributes: map[string]string{"recent.key": "1"},
		},
		log.LogRecord{
			Timestamp: now.Add(-3 * time.Hour), ObservedTS: now.Add(-3 * time.Hour),
			Severity: log.SevInfo, Body: "row-old", Service: "api",
			LogAttributes: map[string]string{"old.key": "1"},
		},
	)

	base := logsBasePath(project.ID) + "/attr-keys"

	resp := getWithCookie(t, s.srv, base+"?period=1h", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?period=1h status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	var got []attrKeyJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", body, err)
	}
	byKey := map[string]int64{}
	for _, it := range got {
		byKey[it.Key] = it.Count
	}
	if _, ok := byKey["old.key"]; ok {
		t.Errorf("period=1h не должен вернуть old.key (запись за пределами окна): %+v", got)
	}
	if _, ok := byKey["recent.key"]; !ok {
		t.Errorf("period=1h должен вернуть recent.key: %+v", got)
	}

	resp = getWithCookie(t, s.srv, base+"?period=7d", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	got = nil
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", body, err)
	}
	byKey = map[string]int64{}
	for _, it := range got {
		byKey[it.Key] = it.Count
	}
	if _, ok := byKey["old.key"]; !ok {
		t.Errorf("period=7d должен вернуть old.key тоже: %+v", got)
	}
	if _, ok := byKey["recent.key"]; !ok {
		t.Errorf("period=7d должен вернуть recent.key тоже: %+v", got)
	}
}

func TestWebLogsAttrKeysAutocompleteNilLogQuery404(t *testing.T) {
	s := newLogsStack(t, false)
	_, ownerCookie, project := newLogsProject(t, s, "logs-attrkeys-noquery-owner@example.com", "logs-attrkeys-noquery-co", "logs-attrkeys-noquery-proj")

	resp := getWithCookie(t, s.srv, logsBasePath(project.ID)+"/attr-keys?q=a", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("h.LogQuery=nil status = %d, want 404", resp.StatusCode)
	}
}

func TestWebLogsAttrFacetsExpandedKeyOutsideTop(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-attrtop-owner@example.com", "logs-attrtop-co", "logs-attrtop-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	var records []log.LogRecord
	for i := 0; i < 20; i++ {
		key := "common.key" + strconv.Itoa(i)
		for j := 0; j < 3; j++ {
			records = append(records, log.LogRecord{
				Timestamp: now.Add(-time.Duration(i*3+j+1) * time.Minute), ObservedTS: now,
				Severity: log.SevInfo, Body: "row-common", Service: "api",
				LogAttributes: map[string]string{key: "v"},
			})
		}
	}
	records = append(records, log.LogRecord{
		Timestamp: now.Add(-500 * time.Millisecond), ObservedTS: now,
		Severity: log.SevInfo, Body: "row-rare", Service: "api",
		LogAttributes: map[string]string{"rare.key": "rare-value"},
	})
	s.seedLogs(t, project.ID, records...)

	base := logsBasePath(project.ID)
	resp := getWithCookie(t, s.srv, base, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	attrItems := logFacetItems(t, string(body), 3)
	if _, ok := findFacetItem(attrItems, "rare.key"); ok {
		t.Fatalf("rare.key не должен быть виден в топ-N сайдбара (тест сам себя не проверяет): %+v", attrItems)
	}

	resp = getWithCookie(t, s.srv, base+"/attr-keys?q=rare.", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var found []attrKeyJSON
	if err := json.Unmarshal(body, &found); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", body, err)
	}
	if len(found) != 1 || found[0].Key != "rare.key" {
		t.Fatalf("attr-keys q=rare. = %+v, want [{rare.key 1}]", found)
	}

	resp = getWithCookie(t, s.srv, base+"?facet=rare.key", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?facet=rare.key status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	expandedItems := logFacetItems(t, string(body), 3)
	keyItem, ok := findFacetItem(expandedItems, "rare.key")
	if !ok {
		t.Fatalf("раскрытый ?facet=rare.key должен появиться в сайдбаре, хотя вне топ-N: %+v", expandedItems)
	}
	if !keyItem.Active {
		t.Fatalf("раскрытый rare.key должен быть отмечен активным: %+v", keyItem)
	}
	valueItem, ok := findFacetItem(expandedItems, "rare-value")
	if !ok || valueItem.Count != "1" {
		t.Fatalf("значение rare-value раскрытого rare.key не отрендерилось: %+v", expandedItems)
	}
}

func TestWebLogsTraceIDFilterChip(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-tracechip-owner@example.com", "logs-tracechip-co", "logs-tracechip-proj")

	const traceID = "abcd1234ef567890"
	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{
			Timestamp: now.Add(-time.Minute), ObservedTS: now.Add(-time.Minute),
			Severity: log.SevInfo, Body: "row-in-trace", Service: "api",
			TraceID: traceID,
		},
		log.LogRecord{
			Timestamp: now.Add(-2 * time.Minute), ObservedTS: now.Add(-2 * time.Minute),
			Severity: log.SevInfo, Body: "row-without-trace", Service: "worker",
		},
	)

	base := logsBasePath(project.ID)
	resp := getWithCookie(t, s.srv, base+"?trace_id="+traceID, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?trace_id=%s status = %d, want 200: %s", base, traceID, resp.StatusCode, body)
	}
	text := string(body)

	if strings.Contains(text, "row-without-trace") {
		t.Errorf("trace_id=%s не сузил список: %s", traceID, text)
	}
	if !strings.Contains(text, "row-in-trace") {
		t.Errorf("trace_id=%s потерял свою же строку: %s", traceID, text)
	}

	if !strings.Contains(text, "abcd1234") {
		t.Errorf("чип с укороченным trace_id не найден: %s", text)
	}

	svcItems := logFacetItems(t, text, 1)
	svcAPI, ok := findFacetItem(svcItems, "api")
	if !ok {
		t.Fatalf("service facet: api не найден: %+v", svcItems)
	}
	if !strings.Contains(svcAPI.Href, "trace_id="+traceID) {
		t.Errorf("facet-ссылка service=api не несёт trace_id: %s", svcAPI.Href)
	}

	removeRe := regexp.MustCompile(`<a class="chip-remove" href="([^"]+)"[^>]*title="[^"]*"`)
	m := removeRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("не нашли ссылку снятия чипа trace_id: %s", text)
	}
	removeHref := html.UnescapeString(m[1])
	if strings.Contains(removeHref, "trace_id") {
		t.Errorf("ссылка снятия чипа не должна содержать trace_id: %s", removeHref)
	}
	if !strings.HasPrefix(removeHref, base) {
		t.Errorf("ссылка снятия чипа должна вести на %s, получили %s", base, removeHref)
	}

	resp = getWithCookie(t, s.srv, removeHref, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (снятие чипа) status = %d, want 200: %s", removeHref, resp.StatusCode, body)
	}
	text = string(body)
	if !strings.Contains(text, "row-without-trace") {
		t.Errorf("после снятия чипа список должен снова показывать все строки: %s", text)
	}
}

func TestWebLogsAttrFilterChip(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-attrchip-owner@example.com", "logs-attrchip-co", "logs-attrchip-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{
			Timestamp: now.Add(-time.Minute), ObservedTS: now.Add(-time.Minute),
			Severity: log.SevInfo, Body: "row-on-host", Service: "api",
			ResourceAttrs: map[string]string{"host.name": "web-01"},
		},
		log.LogRecord{
			Timestamp: now.Add(-2 * time.Minute), ObservedTS: now.Add(-2 * time.Minute),
			Severity: log.SevInfo, Body: "row-other-host", Service: "api",
			ResourceAttrs: map[string]string{"host.name": "web-02"},
		},
	)

	base := logsBasePath(project.ID)
	resp := getWithCookie(t, s.srv, base+"?attr=res:host.name:web-01", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?attr=res:host.name:web-01 status = %d, want 200: %s", base, resp.StatusCode, body)
	}
	text := string(body)

	if strings.Contains(text, "row-other-host") {
		t.Errorf("attr host.name=web-01 не сузил список: %s", text)
	}
	if !strings.Contains(text, "row-on-host") {
		t.Errorf("attr host.name=web-01 потерял свою же строку: %s", text)
	}

	if !strings.Contains(text, "host.name: web-01") {
		t.Errorf("чип attr-фильтра host.name не найден: %s", text)
	}

	removeRe := regexp.MustCompile(`<a class="chip-remove" href="([^"]+)"`)
	m := removeRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("не нашли ссылку снятия attr-чипа: %s", text)
	}
	removeHref := html.UnescapeString(m[1])
	if strings.Contains(removeHref, "host.name") {
		t.Errorf("ссылка снятия attr-чипа не должна содержать host.name: %s", removeHref)
	}

	resp = getWithCookie(t, s.srv, removeHref, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (снятие attr-чипа) status = %d, want 200: %s", removeHref, resp.StatusCode, body)
	}
	if text := string(body); !strings.Contains(text, "row-other-host") {
		t.Errorf("после снятия attr-чипа список должен снова показывать все строки: %s", text)
	}
}

func TestLogsFormCarriesNonFieldFilters(t *testing.T) {
	s := newLogsStack(t, true)
	projectID, cookie, _ := newLogsProject(t, s, "form@example.com", "form-org", "form-proj")

	path := logsBasePath(projectID) +
		"?trace_id=abc123&attr=source%3Anginx&q_not=buffered&service_not=cron"
	resp := getWithCookie(t, s.srv, path, cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, want := range []string{
		`name="trace_id"`, `value="abc123"`,
		`name="attr"`, `value="source:nginx"`,
		`name="q_not"`, `value="buffered"`,
		`name="service_not"`, `value="cron"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("форма не переносит %s — сабмит «Применить» потеряет это условие", want)
		}
	}

	formStart := strings.Index(page, `<form method="get"`)
	if formStart < 0 {
		t.Fatalf("не нашли форму фильтров логов в разметке")
	}
	formEndRel := strings.Index(page[formStart:], "</form>")
	if formEndRel < 0 {
		t.Fatalf("не нашли закрывающий </form> формы фильтров логов")
	}
	formEnd := formStart + formEndRel
	formHTML := page[formStart:formEnd]
	for _, want := range []string{`value="abc123"`, `value="source:nginx"`, `name="q_not"`, `name="service_not"`} {
		if !strings.Contains(formHTML, want) {
			t.Errorf("%s лежит вне <form> — сабмит его не отправит: %s", want, formHTML)
		}
	}

	if !strings.Contains(page, "logs-filter-chip--not") {
		t.Errorf("не нашли чип исключения (класс logs-filter-chip--not): %s", page)
	}
}

func TestLogRowHasExcludeLinks(t *testing.T) {
	s := newLogsStack(t, true)
	projectID, cookie, _ := newLogsProject(t, s, "row@example.com", "row-org", "row-proj")

	now := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	s.seedLogs(t, projectID,
		log.LogRecord{
			Timestamp: now, ObservedTS: now,
			Severity: log.SevError, SeverityNumber: 17, SeverityText: "ERROR",
			Body: "boom", Service: "api", Environment: "production",
			LogAttributes: map[string]string{"source": "nginx"},
		},
	)

	resp := getWithCookie(t, s.srv, logsBasePath(projectID), cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	for _, want := range []string{
		"severity_not=error",
		"service_not=api",
		"attr_not=source%3Anginx",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("в строке нет ссылки исключения с %s: %s", want, page)
		}
	}
	if strings.Contains(page, "before=") || strings.Contains(page, "tskip=") {
		t.Errorf("ссылка исключения тащит курсор пагинации — собрана мимо logsPageURLValues: %s", page)
	}
}

func TestLogRowAttrExcludeUsesExplicitOrigin(t *testing.T) {
	s := newLogsStack(t, true)
	projectID, cookie, _ := newLogsProject(t, s, "attrorigin@example.com", "attrorigin-org", "attrorigin-proj")

	now := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	s.seedLogs(t, projectID,
		log.LogRecord{
			Timestamp: now, ObservedTS: now,
			Severity: log.SevInfo, SeverityNumber: 9, SeverityText: "INFO",
			Body: "pooled", Service: "api", Environment: "production",
			LogAttributes: map[string]string{"resource.pool": "db-1"},
			ResourceAttrs: map[string]string{"host.name": "web-1"},
		},
	)

	resp := getWithCookie(t, s.srv, logsBasePath(projectID), cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)

	if !strings.Contains(page, "attr_not=resource.pool%3Adb-1") {
		t.Errorf("лог-атрибут resource.pool должен исключаться как обычный attr (attr_not=resource.pool%%3Adb-1): %s", page)
	}
	if strings.Contains(page, "attr_not=res%3Apool%3Adb-1") {
		t.Errorf("лог-атрибут resource.pool подменён на resource_attr (res:pool) — происхождение восстановлено разбором отображаемого ключа, а не явным полем: %s", page)
	}
	if !strings.Contains(page, "attr_not=res%3Ahost.name%3Aweb-1") {
		t.Errorf("настоящий атрибут ресурса host.name должен исключаться как resource_attr (res:host.name): %s", page)
	}
}

// Превышение потолка attr= отказывает явно: страница не пытается выполнить запрос
// к ClickHouse с сотнями условий, а показывает отказ. Данные не засеяны — если бы
// запрос всё-таки ушёл, страница отдала бы «логов нет», а не карточку отказа.
func TestWebLogsAttrsOverLimitRejectedWithoutQuery(t *testing.T) {
	s := newLogsStack(t, true)
	projectID, cookie, _ := newLogsProject(t, s, "attrlimit@example.com", "attrlimit-org", "attrlimit-proj")

	q := url.Values{}
	for i := 0; i < 21; i++ {
		q.Add("attr", fmt.Sprintf("k%d:v", i))
	}
	resp := getWithCookie(t, s.srv, logsBasePath(projectID)+"?"+q.Encode(), cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	page := string(body)
	if !strings.Contains(page, "Слишком много фильтров по атрибутам") {
		t.Errorf("страница не показывает отказ по потолку attr=: %s", page)
	}
	if strings.Contains(page, "Ничего не подошло под фильтры") || strings.Contains(page, "По выбранным фильтрам") {
		t.Errorf("страница показывает обычную пустую выборку вместо явного отказа: %s", page)
	}
	// Без ссылки назад отказ — тупик: URL с ?attr= остаётся в адресной строке и правится
	// только руками. Ссылка должна вести на логи БЕЗ параметров attr, а не повторять текущий URL.
	wantHref := `href="` + logsBasePath(projectID) + `"`
	if !strings.Contains(page, wantHref) || !strings.Contains(page, "Сбросить фильтры") {
		t.Errorf("карточка отказа не даёт выхода на %s без attr=: %s", wantHref, page)
	}
}
