package web_test

import (
	"context"
	"encoding/json"
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

// TestWebLogsListTraceLink — правка ревью UX Important #4: trace_id
// раскрытой строки лога должен вести на реальную страницу трейса
// (/traces/{trace_id}), а не на общий раздел «Производительность» без
// пометки, и появляться независимо от span_id (раньше ссылка требовала ОБА
// поля, хотя у самого трейса своей страницы достаточно trace_id). Заодно
// (правка Minor #2) — таблица атрибутов раскрытой строки должна получить
// заголовок (мёртвый до этой правки ключ "logs.table.attributes").
func TestWebLogsListTraceLink(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-tracelink-owner@example.com", "logs-tracelink-co", "logs-tracelink-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{
			Timestamp: now.Add(-time.Minute), ObservedTS: now.Add(-time.Minute),
			Severity: log.SevInfo, Body: "row-with-trace", Service: "api",
			TraceID:       "trace-abc-123", // без SpanID — ссылка обязана появиться и так
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
	// Именно строка trace_id (не вся страница — в rail-навигации всегда есть
	// своя ссылка "Транзакции" на /projects/{id}/performance, это отдельное
	// и легитимное) не должна вести на общий раздел без пометки.
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

// TestWebLogsListRangeClamped — правка ревью UX Important #2 / ops P2:
// выбранный пресет окна шире срока хранения логов должен показывать явную
// подпись о клампе, а не молча урезанный список без объяснения.
func TestWebLogsListRangeClamped(t *testing.T) {
	s := newLogsStack(t, true)
	s.h.LogRetentionDays = 3
	_, ownerCookie, project := newLogsProject(t, s, "logs-clamp-owner@example.com", "logs-clamp-co", "logs-clamp-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.seedLogs(t, project.ID,
		log.LogRecord{Timestamp: now.Add(-time.Minute), ObservedTS: now.Add(-time.Minute), Severity: log.SevInfo, Body: "row-recent", Service: "api"},
	)

	base := logsBasePath(project.ID)

	// period=30d шире retentionDays=3 — подпись обязана появиться.
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

	// period=1h короче retentionDays=3 — клампа нет, подписи тоже.
	resp = getWithCookie(t, s.srv, base+"?period=1h", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if strings.Contains(text, "logs-range-clamped") {
		t.Errorf("period=1h короче retentionDays=3, подписи о клампе быть не должно: %s", text)
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

// TestWebLogsListAttrFacets — задача 5 плана C2: сайдбар атрибут-фасетов
// (4-я секция, после severity/service/environment, см. logAttrFacetSection в
// logs.templ) — авто-обнаруженные ключи со счётчиками видны сразу; клик по
// ключу (переход по СГЕНЕРИРОВАННОЙ ссылке ?facet=<key>) раскрывает его
// значения; клик по значению добавляет точечный фильтр и сужает список;
// повторный клик по уже активному значению снимает фильтр.
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

	// Сайдбар (4-я секция, индекс 3): ключ http.method виден сразу со
	// счётчиком, значения ещё не раскрыты (ссылка на раскрытие).
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

	// Клик по ключу (сгенерированная ссылка ?facet=http.method) раскрывает
	// значения GET/POST со своими counts.
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

	// Клик по значению GET (сгенерированная ссылка) сужает список до
	// log_attributes[http.method]=GET и помечает значение активным.
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

	// Повторный клик по уже активному значению GET снимает фильтр — список
	// возвращается к полному набору http.method.
	resp = getWithCookie(t, s.srv, getAfter.Href, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	text = string(body)
	if !strings.Contains(text, "row-post") {
		t.Errorf("повторный клик по активному GET не снял фильтр: %s", text)
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

// attrKeyJSON — форма одного элемента ответа logsAttrKeys (см.
// web.attrKeyJSON) — тест собственную копию не импортирует (неэкспортируемый
// тип другого пакета), декодирует в такую же структуру по контракту JSON.
type attrKeyJSON struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// TestWebLogsAttrKeysAutocomplete — задача 6 плана C2, §6 спеки: JSON-
// эндпоинт GET /projects/{id}/logs/attr-keys?q=<prefix> фильтрует по
// префиксу ключа, чужой проект → 404, неавторизованный → редирект на
// /login, стенд без проводки логов (h.LogQuery==nil) → 404 (тот же гейт,
// что у самого списка).
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

	// Префикс "http." отфильтровывает db.statement.
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

	// Без q — все обнаруженные ключи, включая db.statement.
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

	// Чужой (не член организации) → 404.
	_, outsider := orgSettingsRegister(t, s.auth, "logs-attrkeys-outsider@example.com")
	resp = getWithCookie(t, s.srv, base+"?q=http.", outsider)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider status = %d, want 404", resp.StatusCode)
	}

	// Неавторизованный → редирект на /login.
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

// TestWebLogsAttrKeysAutocompleteWindow — правка ревью UX Important #3 (§6
// спеки, C2): автокомплит ключей атрибутов ищет в ТЕКУЩЕМ окне фильтра
// (period=/start=/end= из адресной строки, дописывает logs.js), а не в
// фиксированных последних 24ч. Узкое окно (period=1h) не должно вернуть
// ключ записи трёхчасовой давности — иначе подсказка вела бы к ключу,
// которого в видимой при этом окне выборке нет; широкое окно (7d) видит
// обе записи.
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

	// Узкое окно (последний час) — виден только recent.key.
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

	// Широкое окно (7d) — виден и старый ключ тоже (и другой ключ кеша, чем у
	// period=1h выше — не должно склеиться с уже закешированным ответом).
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

// TestWebLogsAttrKeysAutocompleteNilLogQuery404 — тот же гейт, что у
// logsList: без проводки логов эндпоинт отдаёт 404, а не паникует.
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

// TestWebLogsAttrFacetsExpandedKeyOutsideTop — carry-fix из ревью задачи T5
// (§6 спеки, задача 6): раскрытый в URL ключ (?facet=<key>), найденный
// автокомплитом, но не входящий в топ-N сайдбара (logsAttrKeysLimit=20),
// всё равно должен посчитаться и отрендериться со своими значениями — иначе
// «кликнул из автокомплита — ничего не раскрылось» (см. NewAttrFacets в
// logs.templ). Здесь topN намеренно исчерпан 20 РАЗНЫМИ ключами большей
// частоты, а искомый rare.key — 21-й, редкий, гарантированно вне топа.
func TestWebLogsAttrFacetsExpandedKeyOutsideTop(t *testing.T) {
	s := newLogsStack(t, true)
	_, ownerCookie, project := newLogsProject(t, s, "logs-attrtop-owner@example.com", "logs-attrtop-co", "logs-attrtop-proj")

	now := time.Now().UTC().Truncate(time.Millisecond)
	var records []log.LogRecord
	// 20 частых ключей (по 3 записи каждый) занимают весь топ сайдбара.
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
	// rare.key — редкий, один раз, гарантированно вне топ-20 по count DESC.
	records = append(records, log.LogRecord{
		Timestamp: now.Add(-500 * time.Millisecond), ObservedTS: now,
		Severity: log.SevInfo, Body: "row-rare", Service: "api",
		LogAttributes: map[string]string{"rare.key": "rare-value"},
	})
	s.seedLogs(t, project.ID, records...)

	// Сайдбар не показывает rare.key в списке ключей (он вне топ-20).
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

	// Автокомплит его находит.
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

	// Клик по найденному автокомплитом ключу (?facet=rare.key) — carry-fix:
	// значение rare-value должно отрендериться, хотя ключ вне топ-N.
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

// TestWebLogsTraceIDFilterChip — задача 2 плана C3: экран /logs принимает
// ?trace_id=, показывает его снимаемым чипом (укороченный id) и переносит
// параметр в ссылки фасетов сайдбара (не только пагинацию — logsPageURLValues
// единая точка сборки для обеих). Ссылка снятия чипа ведёт на тот же экран
// БЕЗ trace_id.
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

	// Список сужен до строки этого trace_id (Task 1, query-слой).
	if strings.Contains(text, "row-without-trace") {
		t.Errorf("trace_id=%s не сузил список: %s", traceID, text)
	}
	if !strings.Contains(text, "row-in-trace") {
		t.Errorf("trace_id=%s потерял свою же строку: %s", traceID, text)
	}

	// Чип виден с укороченным id.
	if !strings.Contains(text, "abcd1234") {
		t.Errorf("чип с укороченным trace_id не найден: %s", text)
	}

	// Facet-ссылки (сайдбар) несут trace_id дальше — фасет service гарантированно
	// есть (одна строка в скоупе trace_id, service=api).
	svcItems := logFacetItems(t, text, 1)
	svcAPI, ok := findFacetItem(svcItems, "api")
	if !ok {
		t.Fatalf("service facet: api не найден: %+v", svcItems)
	}
	if !strings.Contains(svcAPI.Href, "trace_id="+traceID) {
		t.Errorf("facet-ссылка service=api не несёт trace_id: %s", svcAPI.Href)
	}

	// Ссылка снятия чипа ведёт на тот же экран логов БЕЗ trace_id.
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

	// Переход по ссылке снятия чипа реально убирает скоуп по trace_id.
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

// TestWebLogsAttrFilterChip — аудит UX P1: ссылка «Логи хоста» ставит
// resource-attr фильтр ?attr=res:host.name:X. До фикса он был НЕВИДИМ (чип
// показывался только для trace_id). Теперь активный attr-фильтр (в т.ч.
// resource, у которого фасета в сайдбаре нет) рендерится снимаемым чипом.
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

	// Список сужен до строки этого хоста (существующий attr-фильтр C2).
	if strings.Contains(text, "row-other-host") {
		t.Errorf("attr host.name=web-01 не сузил список: %s", text)
	}
	if !strings.Contains(text, "row-on-host") {
		t.Errorf("attr host.name=web-01 потерял свою же строку: %s", text)
	}

	// Чип активного attr-фильтра виден с подписью "host.name: web-01".
	if !strings.Contains(text, "host.name: web-01") {
		t.Errorf("чип attr-фильтра host.name не найден: %s", text)
	}

	// Ссылка снятия чипа ведёт на тот же экран БЕЗ этого attr.
	removeRe := regexp.MustCompile(`<a class="chip-remove" href="([^"]+)"`)
	m := removeRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("не нашли ссылку снятия attr-чипа: %s", text)
	}
	removeHref := html.UnescapeString(m[1])
	if strings.Contains(removeHref, "host.name") {
		t.Errorf("ссылка снятия attr-чипа не должна содержать host.name: %s", removeHref)
	}

	// Переход по снятию реально убирает attr-скоуп.
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
