package web_test

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
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

// logRowBodyRe вытаскивает тело строки лога из <summary> раскрытия
// (logRowView в logs.templ — <summary>{ logBodyPreview(...) }</summary>, БЕЗ
// атрибутов). Осознанно НЕ используем плоский "(?s)<summary>(.*?)</summary>"
// по всему документу: у шаблона хватает других plain <summary> без класса —
// переключатель проекта и в сайдбаре, и в мобильном меню (details
// class="proj-switch"><summary>...) рендерят его ДВАЖДЫ на каждой странице.
// Поэтому сначала вырезаем <tbody>...</tbody> (см. logRowsTBodyRe), и уже
// внутри него ищем <summary>.
var logRowsTBodyRe = regexp.MustCompile(`(?s)<tbody>(.*?)</tbody>`)
var logRowBodyRe = regexp.MustCompile(`<summary>([^<]*)</summary>`)

// olderHrefRe вытаскивает href ссылки «показать старее» — единственный
// <nav class="pagination"> на странице логов (см. logs.templ, LogsScreen).
var olderHrefRe = regexp.MustCompile(`<nav class="pagination"[^>]*><a href="([^"]+)">`)

// beforeParamRe — значение ?before= в извлечённой ссылке, для подсчёта, сколько
// страниц подряд идут с ОДНИМ И ТЕМ ЖЕ курсором (тай растягивается больше чем
// на страницу — именно этот путь ловит off-by-one в накоплении TieSkip).
var beforeParamRe = regexp.MustCompile(`before=(\d+)`)

// logRowBodiesOnPage возвращает тела строк лога, показанных на текущей
// странице (см. logRowBodyRe), пусто — если таблицы на странице нет
// (пустое состояние).
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

// TestWebLogsListCursorNoDupNoLoss — курсор «показать старее», пройденный по
// РЕАЛЬНО СГЕНЕРИРОВАННОЙ хендлером ссылке (не собранной вручную в тесте, как
// в TestWebLogsListCursorPagination выше): тай-группа (250 строк на одной
// timestamp) больше лимита страницы (100) и заведомо растягивается на
// НЕСКОЛЬКО страниц подряд с ОДНИМ И ТЕМ ЖЕ Before и растущим TieSkip — именно
// эта конфигурация ловит off-by-one в накоплении (web.nextLogCursor). Полное
// прохождение курсора обязано покрыть весь засеянный набор без дублей и без
// потерь.
func TestWebLogsListCursorNoDupNoLoss(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-walk-owner@example.com", "logs-walk-co", "logs-walk-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)

	const distinctCount = 5
	const tieCount = 250 // >> logsListLimit(100) — гарантированно многостраничный тай

	want := map[string]bool{}
	var records []log.LogRecord

	// Самые свежие distinctCount строк — каждая на своей отметке времени, не
	// участвуют в тай-группе; должны попасть на первую страницу вместе с
	// частью тай-группы (тай — сразу следом, старше).
	for i := 0; i < distinctCount; i++ {
		ts := now.Add(-time.Duration(i) * time.Second)
		body := "row-" + strconv.Itoa(i)
		records = append(records, log.LogRecord{Timestamp: ts, ObservedTS: ts, Severity: log.SevInfo, Body: body})
		want[body] = true
	}

	// Тай-группа: все tieCount строк на ОДНОЙ отметке времени, старше всех
	// distinctCount — ложится ровно на границу первой страницы и растягивается
	// на несколько последующих (лимит 100 << 250).
	tieTS := now.Add(-time.Duration(distinctCount+1) * time.Second)
	for i := 0; i < tieCount; i++ {
		body := "tie-" + strconv.Itoa(i)
		records = append(records, log.LogRecord{Timestamp: tieTS, ObservedTS: tieTS, Severity: log.SevInfo, Body: body})
		want[body] = true
	}

	// Хвостовая строка старше тай-группы — маркер «страниц больше нет».
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
			break // страниц больше нет
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

// logFacetSectionRe вырезает одну секцию сайдбара фасетов (logFacetSection в
// logs.templ) — секции идут в фиксированном порядке severity/service/
// environment (см. logFacetsSidebar), поэтому индекс совпадения однозначно
// говорит, какой это фасет, без завязки на локализованный заголовок.
var logFacetSectionRe = regexp.MustCompile(`(?s)<section class="card logs-facet">(.*?)</section>`)

// logFacetItemRe вытаскивает одно значение фасета внутри секции: класс
// ссылки (logs-facet-value или logs-facet-value logs-facet-value-active),
// href, метку и count (logFacetSection в logs.templ).
var logFacetItemRe = regexp.MustCompile(`<a class="(logs-facet-value[^"]*)" href="([^"]+)"[^>]*>([^<]*)</a>\s*<span class="logs-facet-count">(\d+)</span>`)

type logFacetItem struct {
	Active bool
	Href   string
	Label  string
	Count  string
}

// logFacetItems разбирает N-ю (0-based) секцию сайдбара фасетов из HTML
// страницы логов.
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

// findSeverityInfoItem — значение фасета severity для уровня "info": метка
// уже локализована (severityLabel), а тест не знает заранее, ru или en
// отдаёт стенд по умолчанию (тот же приём, что и остальные тесты файла,
// проверяющие оба варианта литералом).
func findSeverityInfoItem(items []logFacetItem) (logFacetItem, bool) {
	if it, ok := findFacetItem(items, "Info"); ok {
		return it, true
	}
	return findFacetItem(items, "Инфо")
}

// TestWebLogsListFacets — задача 4 плана C2: встроенные фасеты severity/
// service/environment в сайдбаре — counts, отсутствие активной метки без
// фильтров, клик по значению (переход по сгенерированной ссылке) реально
// сужает список и помечает значение активным.
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

	// Клик по значению "worker" фасета service — переход по СГЕНЕРИРОВАННОЙ
	// ссылке (не собранной вручную в тесте) должен сузить список до
	// service=worker и пометить это значение активным.
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

	// exclude-self: ?severity=error всё равно показывает ВСЕ уровни в фасете
	// severity (не только error) — счётчик info не должен упасть до 0.
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
