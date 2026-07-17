package telemetry_test

import (
	"context"
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
	// web_vitals_5m — MV: наполняется вставкой транзакции с measurements.
	seedWebVitals(t, ctx, conn, p1, ts)
	seedWebVitals(t, ctx, conn, p2, ts)

	p := telemetry.NewPurger(conn)
	if err := p.PurgeProject(ctx, p1); err != nil {
		t.Fatalf("PurgeProject(p1): %v", err)
	}

	// mutations_sync=2 делает ALTER синхронным — результат детерминирован.
	for _, tbl := range []string{"events", "transactions", "spans", "metric_points", "profile_samples", "check_results", "web_vitals_5m"} {
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
	if err := p.PurgeSubject(ctx, p2, telemetry.Subject{Email: "a@b.com"}); err != nil {
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
	if err := p.PurgeSubject(ctx, p2, telemetry.Subject{UserID: "other"}); err != nil {
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
	if err := p.PurgeSubject(ctx, p2, telemetry.Subject{UserID: "victim", Email: "a@b.com"}); err != nil {
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

func seedSpans(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		"INSERT INTO spans (project_id, timestamp) VALUES (?, ?)", projectID, ts); err != nil {
		t.Fatalf("insert spans: %v", err)
	}
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
