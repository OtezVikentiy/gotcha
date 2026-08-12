package web_test

// TestStatusPageQueriesDoNotGrowWithMonitors: сборка страницы шла циклом по
// мониторам с тремя запросами на каждый (h.Uptime.Get, тяжёлый 90-дневный
// агрегат h.UptimeQuery.Uptime, h.Uptime.IncidentsForMonitor). Сорок
// мониторов и пять еженедельных окон обслуживания упирались в таймаут сборки,
// и посетитель неаутентифицированной страницы получал ошибку — а прогрев
// кеша после каждого истечения TTL достаётся случайному первому посетителю.
//
// Считаем РЕАЛЬНЫЕ обращения к PostgreSQL и ClickHouse, а не вызовы методов
// сервиса: h.Uptime/h.UptimeQuery в Handler — конкретные *uptime.Service/
// *uptime.Query, не интерфейсы (в отличие, например, от Evaluator.Regressions
// в internal/trace, который ради этого же измерения задачи 5 подпроекта
// стал интерфейсом) — подменять их поле в Handler не входит в задачу и
// затронуло бы весь web-пакет. pgx.QueryTracer и обёртка над
// driver.Conn (ClickHouse) считают запросы на уровне драйвера, не меняя ни
// одной сигнатуры продуктового кода.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// countingTracer — pgx.QueryTracer, считающий Query/QueryRow/Exec (все три
// проходят через TraceQueryStart, см. её докстринг в pgx v5).
type countingTracer struct {
	mu sync.Mutex
	n  int
}

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *countingTracer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// countingCHConn считает Query/QueryRow к ClickHouse. driver.Conn встроен
// как поле: Go делегирует все методы интерфейса, не переопределённые ниже,
// самому embedded-значению — не нужно реализовывать вручную все 11 методов
// driver.Conn ради двух, которые использует internal/uptime.Query.
type countingCHConn struct {
	driver.Conn
	mu sync.Mutex
	n  int
}

func (c *countingCHConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Conn.Query(ctx, query, args...)
}

func (c *countingCHConn) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Conn.QueryRow(ctx, query, args...)
}

func (c *countingCHConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// countingStack — тот же набор сервисов, что и statusPageStack
// (statuspage_test.go), но PG-пул и CH-соединение инструментированы для
// подсчёта запросов. Собирается вручную (не через testenv.MigratedPG/
// MigratedCH), потому что тем функциям некуда передать наш tracer/обёртку —
// им для этого нужен голый DSN, который они и дают отдельным экспортом.
type countingStack struct {
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	uptime *uptime.Service
	writer *uptime.ResultWriter
	pg     *countingTracer
	ch     *countingCHConn
}

func newCountingStatusPageStack(t *testing.T) *countingStack {
	t.Helper()
	ctx := context.Background()

	pgDSN := testenv.PostgresDSN(t)
	if err := db.MigratePG(pgDSN); err != nil {
		t.Fatalf("migrate pg: %v", err)
	}
	pgCfg, err := pgxpool.ParseConfig(pgDSN)
	if err != nil {
		t.Fatalf("parse pg config: %v", err)
	}
	tracer := &countingTracer{}
	pgCfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, pgCfg)
	if err != nil {
		t.Fatalf("pg pool: %v", err)
	}
	t.Cleanup(pool.Close)

	chDSN := testenv.ClickHouseDSN(t)
	if err := db.MigrateCH(chDSN); err != nil {
		t.Fatalf("migrate ch: %v", err)
	}
	rawCH, err := db.NewClickHouse(ctx, chDSN)
	if err != nil {
		t.Fatalf("ch conn: %v", err)
	}
	chConn := &countingCHConn{Conn: rawCH}

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query

	uptimeSvc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(chConn)
	go writer.Run()
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(cctx)
	})

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	h.Uptime = uptimeSvc
	h.UptimeWriter = writer
	h.UptimeQuery = uptime.NewQuery(chConn)
	h.Register(mux)

	return &countingStack{srv: srv, org: orgSvc, auth: authSvc, uptime: uptimeSvc, writer: writer, pg: tracer, ch: chConn}
}

func (s *countingStack) totalQueries() int {
	return s.pg.count() + s.ch.count()
}

// buildCountingStatusPage заводит организацию, проект, n мониторов и
// публичную статус-страницу со всеми ними, затем один раз запрашивает
// страницу анонимно и возвращает число запросов к PG+CH, потраченных на
// сборку (запросы регистрации/создания сущностей в счёт не идут — счётчики
// обнуляются после сеттапа и до самого запроса страницы). Данные проверок
// намеренно не сеются: числу запросов, которое проверяет этот тест, важен
// сам факт сборки страницы, а не объём данных в ней.
func buildCountingStatusPage(t *testing.T, n int) (status int, queries int) {
	t.Helper()
	s := newCountingStatusPageStack(t)
	ctx := context.Background()

	ownerID, _ := orgSettingsRegister(t, s.auth, "spqc@example.com")
	o, err := s.org.CreateOrg(ctx, "spqc-org", "SPQC", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(ctx, o.ID, "spqc-proj", "SPQC Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	spMonitors := make([]uptime.StatusPageMonitor, 0, n)
	for i := 0; i < n; i++ {
		m := baseMonitor(proj.ID, "mon")
		m.Config = monHTTPConfig(t, "https://example.com/health")
		created, err := s.uptime.Create(ctx, m, []string{"local"}, nil)
		if err != nil {
			t.Fatalf("create monitor %d: %v", i, err)
		}
		spMonitors = append(spMonitors, uptime.StatusPageMonitor{
			MonitorID: created.ID, DisplayName: "Mon", Position: i,
		})
	}
	sp, err := s.uptime.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: proj.ID, Title: "SPQC Status", Enabled: true,
	}, spMonitors)
	if err != nil {
		t.Fatalf("create status page: %v", err)
	}

	// Счётчик обнуляем ПОСЛЕ сеттапа (регистрация/создание орг/проекта/
	// мониторов/страницы — не то, что измеряет тест) и до единственного
	// анонимного запроса страницы.
	s.pg.mu.Lock()
	s.pg.n = 0
	s.pg.mu.Unlock()
	s.ch.mu.Lock()
	s.ch.n = 0
	s.ch.mu.Unlock()

	statusCode, _ := getAnon(t, s.srv, "/status/"+sp.PublicID)
	return statusCode, s.totalQueries()
}

func TestStatusPageQueriesDoNotGrowWithMonitors(t *testing.T) {
	const n = 5
	statusN, queriesN := buildCountingStatusPage(t, n)
	if statusN != http.StatusOK {
		t.Fatalf("GET /status (n=%d) = %d, want 200", n, statusN)
	}

	statusDouble, queriesDouble := buildCountingStatusPage(t, 2*n)
	if statusDouble != http.StatusOK {
		t.Fatalf("GET /status (n=%d) = %d, want 200", 2*n, statusDouble)
	}

	t.Logf("queries: n=%d -> %d, n=%d -> %d", n, queriesN, 2*n, queriesDouble)

	// Не привязываемся к точному числу запросов (изменится от любой невинной
	// правки) — проверяем, что удвоение числа мониторов не даёт
	// пропорционального роста. Цикл по мониторам с тремя запросами на монитор
	// давал бы queriesDouble примерно вдвое больше queriesN (плюс те же
	// накладные); допуск +3 покрывает любые мелкие фиксированные различия
	// пакетных запросов, но не позволяет росту, кратному числу мониторов.
	if queriesDouble > queriesN+3 {
		t.Fatalf("запросов при n=%d: %d; при n=%d: %d — рост пропорционален числу мониторов, "+
			"сборка страницы всё ещё делает запрос на монитор вместо пакетного",
			n, queriesN, 2*n, queriesDouble)
	}
}
