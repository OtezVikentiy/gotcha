package telemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// count возвращает число строк в таблице по project_id.
func count(t *testing.T, ctx context.Context, conn driver.Conn, table string, projectID int64) uint64 {
	t.Helper()
	var n uint64
	// Имена таблиц — из теста, не из пользовательского ввода.
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+table+" WHERE project_id = ?", projectID).Scan(&n); err != nil {
		t.Fatalf("count %s p%d: %v", table, projectID, err)
	}
	return n
}

// countEventsByEmail возвращает число событий проекта с указанным user_email.
func countEventsByEmail(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, email string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx,
		"SELECT count() FROM events WHERE project_id = ? AND user_email = ?", projectID, email).Scan(&n); err != nil {
		t.Fatalf("count events by email: %v", err)
	}
	return n
}

func TestPurgeProject(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p1 = int64(1)
	const p2 = int64(2)
	ts := time.Now().UTC()

	// Наполняем все таблицы, которые чистит PurgeProject, для двух проектов.
	seedEvents(t, ctx, conn, p1, "u1", "10.0.0.1", "a@b.com", ts)
	seedEvents(t, ctx, conn, p2, "u2", "10.0.0.2", "c@d.com", ts)
	seedTransactions(t, ctx, conn, p1, "u1", ts)
	seedTransactions(t, ctx, conn, p2, "u2", ts)
	seedSpans(t, ctx, conn, p1, ts)
	seedSpans(t, ctx, conn, p2, ts)
	seedMetricPoints(t, ctx, conn, p1, ts)
	seedMetricPoints(t, ctx, conn, p2, ts)
	seedProfileSamples(t, ctx, conn, p1, ts)
	seedProfileSamples(t, ctx, conn, p2, ts)
	seedCheckResults(t, ctx, conn, p1, ts)
	seedCheckResults(t, ctx, conn, p2, ts)
	seedLogs(t, ctx, conn, p1, ts)
	seedLogs(t, ctx, conn, p2, ts)
	// web_vitals_5m — MV: наполняется вставкой транзакции с measurements.
	seedWebVitals(t, ctx, conn, p1, ts)
	seedWebVitals(t, ctx, conn, p2, ts)

	p := telemetry.NewPurger(conn)
	if err := p.PurgeProject(ctx, p1); err != nil {
		t.Fatalf("PurgeProject(p1): %v", err)
	}

	// mutations_sync=2 делает ALTER синхронным — результат детерминирован.
	for _, tbl := range []string{"events", "transactions", "spans", "metric_points", "profile_samples", "check_results", "logs", "web_vitals_5m"} {
		if got := count(t, ctx, conn, tbl, p1); got != 0 {
			t.Errorf("%s p1: осталось %d строк, ждали 0", tbl, got)
		}
		if got := count(t, ctx, conn, tbl, p2); got == 0 {
			t.Errorf("%s p2: строки удалены, а не должны были", tbl)
		}
	}
}

func TestPurgeSubject(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p1 = int64(10)
	const p2 = int64(20)
	ts := time.Now().UTC()

	// В проекте p2 два субъекта: удаляемый (a@b.com) и посторонний (keep@x.com).
	seedEvents(t, ctx, conn, p1, "u1", "10.0.0.1", "a@b.com", ts) // другой проект — не трогаем
	seedEvents(t, ctx, conn, p2, "victim", "192.168.0.1", "a@b.com", ts)
	seedEvents(t, ctx, conn, p2, "other", "192.168.0.2", "keep@x.com", ts)
	seedTransactions(t, ctx, conn, p2, "victim", ts)
	seedTransactions(t, ctx, conn, p2, "other", ts)

	p := telemetry.NewPurger(conn)
	if _, err := p.PurgeSubject(ctx, p2, telemetry.Subject{Email: "a@b.com"}); err != nil {
		t.Fatalf("PurgeSubject: %v", err)
	}

	// В p2 события с a@b.com удалены, keep@x.com целы.
	if got := countEventsByEmail(t, ctx, conn, p2, "a@b.com"); got != 0 {
		t.Errorf("p2 events a@b.com: осталось %d, ждали 0", got)
	}
	if got := countEventsByEmail(t, ctx, conn, p2, "keep@x.com"); got == 0 {
		t.Errorf("p2 events keep@x.com удалены, а не должны были")
	}
	// Другой проект с тем же email не затронут.
	if got := countEventsByEmail(t, ctx, conn, p1, "a@b.com"); got == 0 {
		t.Errorf("p1 events a@b.com удалены, а не должны были (субъект чистится в рамках проекта)")
	}

	// Теперь чистим субъекта по user_id — уходят и события, и транзакции.
	if _, err := p.PurgeSubject(ctx, p2, telemetry.Subject{UserID: "other"}); err != nil {
		t.Fatalf("PurgeSubject by user_id: %v", err)
	}
	var evLeft, txLeft uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM events WHERE project_id = ? AND user_id = ?", p2, "other").Scan(&evLeft); err != nil {
		t.Fatalf("count events other: %v", err)
	}
	if evLeft != 0 {
		t.Errorf("p2 events user_id=other: осталось %d, ждали 0", evLeft)
	}
	if err := conn.QueryRow(ctx, "SELECT count() FROM transactions WHERE project_id = ? AND user_id = ?", p2, "other").Scan(&txLeft); err != nil {
		t.Fatalf("count tx other: %v", err)
	}
	if txLeft != 0 {
		t.Errorf("p2 transactions user_id=other: осталось %d, ждали 0", txLeft)
	}
	// Транзакция victim осталась (её чистили по email, а транзакции email не содержат).
	var txVictim uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM transactions WHERE project_id = ? AND user_id = ?", p2, "victim").Scan(&txVictim); err != nil {
		t.Fatalf("count tx victim: %v", err)
	}
	if txVictim == 0 {
		t.Errorf("p2 transactions user_id=victim удалены преждевременно")
	}
}

// TestPurgeSubjectMetricPoints проверяет, что PurgeSubject чистит и ПДн субъекта
// из metric_points.attributes (user.id / enduser.id / user.email), не задевая
// метрики постороннего субъекта и чужого проекта.
func TestPurgeSubjectMetricPoints(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p1 = int64(30)
	const p2 = int64(40)
	ts := time.Now().UTC()

	// p2: метрика субъекта (user.id=victim), метрика по enduser.id, метрика по
	// user.email и метрика постороннего. p1 — чужой проект с тем же user.id.
	seedMetricPointAttr(t, ctx, conn, p2, map[string]string{"user.id": "victim"}, ts)
	seedMetricPointAttr(t, ctx, conn, p2, map[string]string{"enduser.id": "victim"}, ts)
	seedMetricPointAttr(t, ctx, conn, p2, map[string]string{"user.email": "a@b.com"}, ts)
	seedMetricPointAttr(t, ctx, conn, p2, map[string]string{"user.id": "other"}, ts)
	seedMetricPointAttr(t, ctx, conn, p1, map[string]string{"user.id": "victim"}, ts)

	p := telemetry.NewPurger(conn)
	if _, err := p.PurgeSubject(ctx, p2, telemetry.Subject{UserID: "victim", Email: "a@b.com"}); err != nil {
		t.Fatalf("PurgeSubject: %v", err)
	}

	// В p2 остались только метрики постороннего субъекта (user.id=other): 1 строка.
	if got := count(t, ctx, conn, "metric_points", p2); got != 1 {
		t.Errorf("p2 metric_points: осталось %d, ждали 1 (только other)", got)
	}
	var otherLeft uint64
	if err := conn.QueryRow(ctx,
		"SELECT count() FROM metric_points WHERE project_id = ? AND attributes['user.id'] = ?", p2, "other").Scan(&otherLeft); err != nil {
		t.Fatalf("count metric_points other: %v", err)
	}
	if otherLeft != 1 {
		t.Errorf("p2 metric_points other: осталось %d, ждали 1", otherLeft)
	}
	// Чужой проект не затронут.
	if got := count(t, ctx, conn, "metric_points", p1); got != 1 {
		t.Errorf("p1 metric_points: осталось %d, ждали 1 (субъект чистится в рамках проекта)", got)
	}
}

// TestPurgeSubjectLogs проверяет, что PurgeSubject чистит ПДн субъекта из
// logs.log_attributes по всем четырём ключам (user.id/enduser.id ← UserID,
// user.email/enduser.email ← Email), не задевая логи постороннего субъекта и
// чужого проекта.
func TestPurgeSubjectLogs(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p1 = int64(60)
	const p2 = int64(70)
	ts := time.Now().UTC()

	// p2: логи субъекта по всем четырём ключам и лог постороннего. p1 — чужой
	// проект с тем же user.id.
	seedLogAttr(t, ctx, conn, p2, map[string]string{"user.id": "victim"}, ts)
	seedLogAttr(t, ctx, conn, p2, map[string]string{"enduser.id": "victim"}, ts)
	seedLogAttr(t, ctx, conn, p2, map[string]string{"user.email": "a@b.com"}, ts)
	seedLogAttr(t, ctx, conn, p2, map[string]string{"enduser.email": "a@b.com"}, ts)
	seedLogAttr(t, ctx, conn, p2, map[string]string{"user.id": "other"}, ts)
	seedLogAttr(t, ctx, conn, p1, map[string]string{"user.id": "victim"}, ts)

	p := telemetry.NewPurger(conn)
	res, err := p.PurgeSubject(ctx, p2, telemetry.Subject{UserID: "victim", Email: "a@b.com"})
	if err != nil {
		t.Fatalf("PurgeSubject: %v", err)
	}
	if res.Logs != 4 {
		t.Errorf("res.Logs = %d, ждали 4 (совпадения по всем четырём ключам)", res.Logs)
	}

	// В p2 остался только лог постороннего (user.id=other): 1 строка.
	if got := count(t, ctx, conn, "logs", p2); got != 1 {
		t.Errorf("p2 logs: осталось %d, ждали 1 (только other)", got)
	}
	var otherLeft uint64
	if err := conn.QueryRow(ctx,
		"SELECT count() FROM logs WHERE project_id = ? AND log_attributes['user.id'] = ?", p2, "other").Scan(&otherLeft); err != nil {
		t.Fatalf("count logs other: %v", err)
	}
	if otherLeft != 1 {
		t.Errorf("p2 logs other: осталось %d, ждали 1", otherLeft)
	}
	// Чужой проект не затронут.
	if got := count(t, ctx, conn, "logs", p1); got != 1 {
		t.Errorf("p1 logs: осталось %d, ждали 1 (субъект чистится в рамках проекта)", got)
	}
}

// TestPurgeSubjectTransactionTags проверяет, что PurgeSubject чистит транзакции,
// где субъект выделяется не колонкой user_id, а тегами (OTLP-приём: user.id/
// enduser.id ← UserID, user.email/enduser.email ← Email), не задевая посторонних.
func TestPurgeSubjectTransactionTags(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p = int64(50)
	ts := time.Now().UTC()

	// Транзакции субъекта victim / a@b.com — по разным конвенциям тегов.
	seedTransactionTags(t, ctx, conn, p, map[string]string{"user.id": "victim"}, ts)
	seedTransactionTags(t, ctx, conn, p, map[string]string{"enduser.id": "victim"}, ts)
	seedTransactionTags(t, ctx, conn, p, map[string]string{"user.email": "a@b.com"}, ts)
	seedTransactionTags(t, ctx, conn, p, map[string]string{"enduser.email": "a@b.com"}, ts)
	// Транзакция субъекта по колонке user_id (Sentry-приём).
	seedTransactions(t, ctx, conn, p, "victim", ts)
	// Посторонние: чужой user.id в тегах и чужой user_id в колонке.
	seedTransactionTags(t, ctx, conn, p, map[string]string{"user.id": "other"}, ts)
	seedTransactions(t, ctx, conn, p, "other", ts)

	purger := telemetry.NewPurger(conn)
	if _, err := purger.PurgeSubject(ctx, p, telemetry.Subject{UserID: "victim", Email: "a@b.com"}); err != nil {
		t.Fatalf("PurgeSubject: %v", err)
	}

	// Должны остаться только 2 транзакции постороннего (тег + колонка).
	if got := count(t, ctx, conn, "transactions", p); got != 2 {
		t.Errorf("transactions p: осталось %d, ждали 2 (только other)", got)
	}
}

// TestPurgeSubjectSpans проверяет косвенное удаление spans субъекта через
// trace_id его транзакций (у spans нет собственной колонки субъекта — см.
// docblock PurgeSubject). Спан постороннего трейса и чужого проекта остаются
// на месте, а "осиротевший" спан без строки в transactions переживает вызов —
// это задокументированная граница механизма, а не брак.
func TestPurgeSubjectSpans(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p1 = int64(80)
	const p2 = int64(81)
	ts := time.Now().UTC()

	// Чужой проект: та же строка trace_id, тот же user_id — не должен быть
	// затронут, потому что project_id обязателен в условии удаления.
	seedTransactionTrace(t, ctx, conn, p1, "victim", "tr-victim", ts)
	seedSpanTrace(t, ctx, conn, p1, "tr-victim", ts)

	// p2: транзакция и спан субъекта.
	seedTransactionTrace(t, ctx, conn, p2, "victim", "tr-victim", ts)
	seedSpanTrace(t, ctx, conn, p2, "tr-victim", ts)
	// p2: транзакция и спан постороннего — должны остаться.
	seedTransactionTrace(t, ctx, conn, p2, "other", "tr-other", ts)
	seedSpanTrace(t, ctx, conn, p2, "tr-other", ts)
	// p2: "осиротевший" спан без строки в transactions — граница механизма:
	// доживает до собственного TTL, а не удаляется вместе с субъектом.
	seedSpanTrace(t, ctx, conn, p2, "tr-orphan", ts)

	p := telemetry.NewPurger(conn)
	res, err := p.PurgeSubject(ctx, p2, telemetry.Subject{UserID: "victim"})
	if err != nil {
		t.Fatalf("PurgeSubject: %v", err)
	}
	if res.Spans != 1 {
		t.Errorf("res.Spans = %d, ждали 1 (спан victim)", res.Spans)
	}
	if res.Total() < res.Spans {
		t.Fatalf("res.Total() = %d меньше res.Spans = %d", res.Total(), res.Spans)
	}

	if got := countSpansByTrace(t, ctx, conn, p2, "tr-victim"); got != 0 {
		t.Errorf("p2 spans tr-victim: осталось %d, ждали 0", got)
	}
	if got := countSpansByTrace(t, ctx, conn, p2, "tr-other"); got == 0 {
		t.Errorf("p2 spans tr-other удалены, а не должны были")
	}
	if got := countSpansByTrace(t, ctx, conn, p2, "tr-orphan"); got == 0 {
		t.Errorf("p2 spans tr-orphan (без строки в transactions) удалены — граница механизма нарушена")
	}
	if got := countSpansByTrace(t, ctx, conn, p1, "tr-victim"); got == 0 {
		t.Errorf("p1 spans tr-victim удалены, а не должны были (субъект чистится в рамках проекта)")
	}
}

// failOnceConn оборачивает driver.Conn и возвращает ошибку на первый Exec,
// чей текст запроса содержит match, а дальше форвардит все вызовы как есть.
// Нужен, чтобы воспроизвести сбой ровно в удалении spans, не трогая остальную
// логику PurgeSubject — остальные методы наследуются встраиванием.
type failOnceConn struct {
	driver.Conn
	match  string
	failed bool
}

func (f *failOnceConn) Exec(ctx context.Context, query string, args ...any) error {
	if !f.failed && strings.Contains(query, f.match) {
		f.failed = true
		return errors.New("injected failure")
	}
	return f.Conn.Exec(ctx, query, args...)
}

// TestPurgeSubjectSpansRetryAfterFailure фиксирует правку по ретраю: spans
// удаляются РАНЬШЕ transactions ровно затем, чтобы сбой на spans не забирал у
// повторного вызова возможность найти trace_id субъекта заново. Если порядок
// когда-нибудь перевернут обратно (transactions раньше spans), тот же сбой на
// spans застанет transactions уже удалёнными — и этот тест поймает это на
// втором ассерте ("transactions удалены несмотря на сбой").
func TestPurgeSubjectSpansRetryAfterFailure(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p = int64(82)
	ts := time.Now().UTC()

	seedTransactionTrace(t, ctx, conn, p, "victim", "tr-retry", ts)
	seedSpanTrace(t, ctx, conn, p, "tr-retry", ts)

	failing := &failOnceConn{Conn: conn, match: "ALTER TABLE spans DELETE"}
	if _, err := telemetry.NewPurger(failing).PurgeSubject(ctx, p, telemetry.Subject{UserID: "victim"}); err == nil {
		t.Fatal("PurgeSubject: ждали ошибку от инъекции сбоя в удалении spans")
	}

	// Сбой на spans не должен успевать стереть transactions — иначе у retry
	// не останется способа заново найти trace_id субъекта.
	if got := count(t, ctx, conn, "transactions", p); got == 0 {
		t.Fatal("transactions удалены несмотря на сбой удаления spans — retry больше невозможен")
	}
	if got := countSpansByTrace(t, ctx, conn, p, "tr-retry"); got == 0 {
		t.Fatal("spans tr-retry удалены несмотря на инъекцию сбоя")
	}

	// Повтор без инъекции сбоя обязан довести дело до конца: найти те же
	// trace_id заново (транзакция ещё жива) и удалить и spans, и transactions.
	res, err := telemetry.NewPurger(conn).PurgeSubject(ctx, p, telemetry.Subject{UserID: "victim"})
	if err != nil {
		t.Fatalf("PurgeSubject retry: %v", err)
	}
	if res.Spans != 1 {
		t.Errorf("retry res.Spans = %d, ждали 1", res.Spans)
	}
	if got := count(t, ctx, conn, "transactions", p); got != 0 {
		t.Errorf("transactions p: осталось %d после retry, ждали 0", got)
	}
	if got := countSpansByTrace(t, ctx, conn, p, "tr-retry"); got != 0 {
		t.Errorf("spans tr-retry: осталось %d после retry, ждали 0", got)
	}
}

// --- helpers наполнения таблиц (только нужные колонки, остальные по умолчанию) ---

func seedEvents(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, userID, ip, email string, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO events (event_id, project_id, issue_id, timestamp, user_id, user_ip, user_email) VALUES (generateUUIDv4(), ?, 1, ?, ?, ?, ?)",
		projectID, ts, userID, ip, email); err != nil {
		t.Fatalf("insert events: %v", err)
	}
}

func seedTransactions(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, userID string, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO transactions (project_id, trace_id, span_id, transaction, timestamp, user_id) VALUES (?, 'tr', 'sp', '/x', ?, ?)",
		projectID, ts, userID); err != nil {
		t.Fatalf("insert transactions: %v", err)
	}
}

// seedTransactionTags вставляет транзакцию с заданными tags (без user_id):
// так OTLP-приём кладёт идентификаторы субъекта — user.id/enduser.id/user.email/
// enduser.email попадают в tags, а не в колонку user_id.
func seedTransactionTags(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, tags map[string]string, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO transactions (project_id, trace_id, span_id, transaction, timestamp, tags) VALUES (?, 'tr', 'sp', '/x', ?, ?)",
		projectID, ts, tags); err != nil {
		t.Fatalf("insert transactions with tags: %v", err)
	}
}

func seedSpans(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO spans (project_id, timestamp) VALUES (?, ?)", projectID, ts); err != nil {
		t.Fatalf("insert spans: %v", err)
	}
}

// seedTransactionTrace вставляет транзакцию с заданными user_id и trace_id —
// нужна там, где важна конкретная связка trace_id (проверка PurgeSubject по
// spans, у которых нет собственной колонки субъекта).
func seedTransactionTrace(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, userID, traceID string, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO transactions (project_id, trace_id, span_id, transaction, timestamp, user_id) VALUES (?, ?, 'sp', '/x', ?, ?)",
		projectID, traceID, ts, userID); err != nil {
		t.Fatalf("insert transactions with trace_id: %v", err)
	}
}

// seedSpanTrace вставляет спан с заданным trace_id.
func seedSpanTrace(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, traceID string, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO spans (project_id, trace_id, timestamp) VALUES (?, ?, ?)", projectID, traceID, ts); err != nil {
		t.Fatalf("insert spans with trace_id: %v", err)
	}
}

// countSpansByTrace возвращает число spans проекта с заданным trace_id.
func countSpansByTrace(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, traceID string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx,
		"SELECT count() FROM spans WHERE project_id = ? AND trace_id = ?", projectID, traceID).Scan(&n); err != nil {
		t.Fatalf("count spans by trace: %v", err)
	}
	return n
}

func seedMetricPoints(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO metric_points (project_id, ts) VALUES (?, ?)", projectID, ts); err != nil {
		t.Fatalf("insert metric_points: %v", err)
	}
}

// seedMetricPointAttr вставляет точку метрики с заданными attributes.
func seedMetricPointAttr(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, attrs map[string]string, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO metric_points (project_id, name, attributes, ts) VALUES (?, 'm', ?, ?)",
		projectID, attrs, ts); err != nil {
		t.Fatalf("insert metric_points with attrs: %v", err)
	}
}

func seedProfileSamples(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO profile_samples (project_id, ts) VALUES (?, ?)", projectID, ts); err != nil {
		t.Fatalf("insert profile_samples: %v", err)
	}
}

func seedCheckResults(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO check_results (monitor_id, project_id, region, timestamp) VALUES (1, ?, 'eu', ?)",
		projectID, ts); err != nil {
		t.Fatalf("insert check_results: %v", err)
	}
}

func seedLogs(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO logs (project_id, timestamp) VALUES (?, ?)", projectID, ts); err != nil {
		t.Fatalf("insert logs: %v", err)
	}
}

// seedLogAttr вставляет строку лога с заданными log_attributes.
func seedLogAttr(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, attrs map[string]string, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO logs (project_id, log_attributes, timestamp) VALUES (?, ?, ?)",
		projectID, attrs, ts); err != nil {
		t.Fatalf("insert logs with attrs: %v", err)
	}
}

// seedWebVitals наполняет MV web_vitals_5m: она агрегирует вставки в transactions
// с непустым measurements, поэтому вставляем транзакцию с lcp.
func seedWebVitals(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO transactions (project_id, trace_id, span_id, transaction, op, timestamp, environment, measurements) "+
			"VALUES (?, 'tr', 'sp', '/wv', 'pageload', ?, 'production', map('lcp', 2000.0))",
		projectID, ts); err != nil {
		t.Fatalf("insert transaction for web_vitals_5m: %v", err)
	}
}

// TestPurgeSubjectReportsMatchedRows фиксирует правку 152-ФЗ: удаление обязано
// сообщать, сколько строк оно затронуло.
//
// Раньше PurgeSubject возвращал только error, и «успех» без единой удалённой
// строки был неотличим от настоящего удаления. Случай не гипотетический, а
// поведение по умолчанию: GOTCHA_SCRUB_IP и GOTCHA_SCRUB_EMAIL включены, значит
// events.user_email и events.user_ip зануляются на приёме и поиск по ним не
// совпадает ни с чем никогда. Владелец орга, исполняющий требование по ст. 14,
// обязан видеть разницу между «удалено N записей» и «не найдено ничего».
func TestPurgeSubjectReportsMatchedRows(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const pid = int64(90210)
	ts := time.Now().UTC()
	seedEvents(t, ctx, conn, pid, "victim", "192.168.0.1", "victim@example.com", ts)
	seedEvents(t, ctx, conn, pid, "other", "192.168.0.2", "keep@example.com", ts)

	p := telemetry.NewPurger(conn)

	// Совпадение есть — счётчик обязан его показать.
	res, err := p.PurgeSubject(ctx, pid, telemetry.Subject{Email: "victim@example.com"})
	if err != nil {
		t.Fatalf("PurgeSubject: %v", err)
	}
	if res.Events == 0 {
		t.Fatal("res.Events = 0, хотя события субъекта были — счётчик не считает")
	}
	if res.Total() < res.Events {
		t.Fatalf("res.Total() = %d меньше res.Events = %d", res.Total(), res.Events)
	}

	// Совпадений нет — ноль, и это НЕ ошибка. Ровно этот исход раньше выглядел
	// как успешное удаление.
	res, err = p.PurgeSubject(ctx, pid, telemetry.Subject{Email: "nobody@example.com"})
	if err != nil {
		t.Fatalf("PurgeSubject (нет совпадений): %v", err)
	}
	if res.Total() != 0 {
		t.Fatalf("res.Total() = %d, want 0", res.Total())
	}
}
