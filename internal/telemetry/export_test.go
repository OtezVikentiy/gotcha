package telemetry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestExportSubject(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p1 = int64(100)
	const p2 = int64(200)
	ts := time.Now().UTC()

	// p2: субъект (victim / a@b.com) и посторонний (other / keep@x.com).
	seedEvents(t, ctx, conn, p1, "victim", "10.0.0.1", "a@b.com", ts) // чужой проект
	seedEvents(t, ctx, conn, p2, "victim", "192.168.0.1", "a@b.com", ts)
	seedEvents(t, ctx, conn, p2, "other", "192.168.0.2", "keep@x.com", ts)
	seedTransactions(t, ctx, conn, p2, "victim", ts)
	seedTransactions(t, ctx, conn, p2, "other", ts)

	p := telemetry.NewPurger(conn)

	exp, err := p.ExportSubject(ctx, p2, telemetry.Subject{Email: "a@b.com"})
	if err != nil {
		t.Fatalf("ExportSubject by email: %v", err)
	}
	if len(exp.Events) != 1 {
		t.Fatalf("events по email: получили %d, ждали 1", len(exp.Events))
	}
	if exp.Events[0].UserEmail != "a@b.com" {
		t.Errorf("events[0].UserEmail=%q, ждали a@b.com", exp.Events[0].UserEmail)
	}
	if exp.Events[0].ProjectID != uint64(p2) {
		t.Errorf("events[0].ProjectID=%d, ждали %d", exp.Events[0].ProjectID, p2)
	}
	if len(exp.Transactions) != 0 {
		t.Errorf("transactions по email: получили %d, ждали 0 (email в transactions не хранится)", len(exp.Transactions))
	}

	exp2, err := p.ExportSubject(ctx, p2, telemetry.Subject{UserID: "victim"})
	if err != nil {
		t.Fatalf("ExportSubject by user_id: %v", err)
	}
	if len(exp2.Events) != 1 {
		t.Errorf("events по user_id: получили %d, ждали 1", len(exp2.Events))
	}
	if len(exp2.Transactions) != 1 {
		t.Errorf("transactions по user_id: получили %d, ждали 1", len(exp2.Transactions))
	}
	if len(exp2.Transactions) == 1 && exp2.Transactions[0].UserID != "victim" {
		t.Errorf("transactions[0].UserID=%q, ждали victim", exp2.Transactions[0].UserID)
	}

	if _, err := p.ExportSubject(ctx, p2, telemetry.Subject{}); err == nil {
		t.Errorf("ExportSubject с пустым субъектом должен вернуть ошибку")
	}
}

func TestExportSubjectTransactionTags(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p = int64(250)
	ts := time.Now().UTC()

	seedTransactionTags(t, ctx, conn, p, map[string]string{"user.email": "a@b.com"}, ts)
	seedTransactionTags(t, ctx, conn, p, map[string]string{"enduser.email": "a@b.com"}, ts)
	seedTransactionTags(t, ctx, conn, p, map[string]string{"user.email": "keep@x.com"}, ts)

	p2 := telemetry.NewPurger(conn)

	exp, err := p2.ExportSubject(ctx, p, telemetry.Subject{Email: "a@b.com"})
	if err != nil {
		t.Fatalf("ExportSubject by email: %v", err)
	}
	if len(exp.Transactions) != 2 {
		t.Errorf("transactions по email в тегах: получили %d, ждали 2", len(exp.Transactions))
	}
}

func TestExportSubjectMetricPoints(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p2 = int64(300)
	ts := time.Now().UTC()

	// p2: метрика субъекта (user.id=victim) и метрика постороннего (user.id=other).
	seedMetricPointAttr(t, ctx, conn, p2, map[string]string{"user.id": "victim"}, ts)
	seedMetricPointAttr(t, ctx, conn, p2, map[string]string{"user.id": "other"}, ts)

	p := telemetry.NewPurger(conn)

	exp, err := p.ExportSubject(ctx, p2, telemetry.Subject{UserID: "victim"})
	if err != nil {
		t.Fatalf("ExportSubject by user_id: %v", err)
	}
	if len(exp.MetricPoints) != 1 {
		t.Fatalf("metric_points по user_id: получили %d, ждали 1", len(exp.MetricPoints))
	}
	if exp.MetricPoints[0].Attributes["user.id"] != "victim" {
		t.Errorf("metric_points[0].attributes[user.id]=%q, ждали victim", exp.MetricPoints[0].Attributes["user.id"])
	}
	if exp.MetricPoints[0].ProjectID != uint64(p2) {
		t.Errorf("metric_points[0].ProjectID=%d, ждали %d", exp.MetricPoints[0].ProjectID, p2)
	}

	expIP, err := p.ExportSubject(ctx, p2, telemetry.Subject{IP: "192.168.0.1"})
	if err != nil {
		t.Fatalf("ExportSubject by IP: %v", err)
	}
	if len(expIP.MetricPoints) != 0 {
		t.Errorf("metric_points по IP: получили %d, ждали 0 (метрики не сегментируются по IP)", len(expIP.MetricPoints))
	}
}

func TestExportSubjectLogs(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p1 = int64(400)
	const p2 = int64(500)
	ts := time.Now().UTC()

	seedLogAttr(t, ctx, conn, p2, map[string]string{"user.id": "victim"}, ts)
	seedLogAttr(t, ctx, conn, p2, map[string]string{"enduser.id": "victim"}, ts)
	seedLogAttr(t, ctx, conn, p2, map[string]string{"user.email": "a@b.com"}, ts)
	seedLogAttr(t, ctx, conn, p2, map[string]string{"enduser.email": "a@b.com"}, ts)
	seedLogAttr(t, ctx, conn, p2, map[string]string{"user.id": "other"}, ts)
	seedLogAttr(t, ctx, conn, p1, map[string]string{"user.id": "victim"}, ts) // чужой проект

	p := telemetry.NewPurger(conn)

	exp, err := p.ExportSubject(ctx, p2, telemetry.Subject{UserID: "victim", Email: "a@b.com"})
	if err != nil {
		t.Fatalf("ExportSubject: %v", err)
	}
	if len(exp.Logs) != 4 {
		t.Fatalf("logs: получили %d, ждали 4 (совпадения по всем четырём ключам)", len(exp.Logs))
	}
	if exp.Logs[0].ProjectID != uint64(p2) {
		t.Errorf("logs[0].ProjectID=%d, ждали %d", exp.Logs[0].ProjectID, p2)
	}

	expIP, err := p.ExportSubject(ctx, p2, telemetry.Subject{IP: "192.168.0.1"})
	if err != nil {
		t.Fatalf("ExportSubject by IP: %v", err)
	}
	if len(expIP.Logs) != 0 {
		t.Errorf("logs по IP: получили %d, ждали 0 (логи не сегментируются по IP)", len(expIP.Logs))
	}
}

func TestExportSubjectSpans(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p1 = int64(700)
	const p2 = int64(701)
	ts := time.Now().UTC()

	// Чужой проект: тот же trace_id, тот же user_id — не должен быть затронут.
	seedTransactionTrace(t, ctx, conn, p1, "victim", "tr-victim", ts)
	seedSpanTrace(t, ctx, conn, p1, "tr-victim", ts)

	seedTransactionTrace(t, ctx, conn, p2, "victim", "tr-victim", ts)
	seedSpanTrace(t, ctx, conn, p2, "tr-victim", ts)
	seedTransactionTrace(t, ctx, conn, p2, "other", "tr-other", ts)
	seedSpanTrace(t, ctx, conn, p2, "tr-other", ts)
	// Осиротевший спан без строки в transactions — вне охвата, как и у PurgeSubject.
	seedSpanTrace(t, ctx, conn, p2, "tr-orphan", ts)

	p := telemetry.NewPurger(conn)
	exp, err := p.ExportSubject(ctx, p2, telemetry.Subject{UserID: "victim"})
	if err != nil {
		t.Fatalf("ExportSubject: %v", err)
	}
	if len(exp.Spans) != 1 {
		t.Fatalf("spans: получили %d, ждали 1 (спан victim)", len(exp.Spans))
	}
	if exp.Spans[0].TraceID != "tr-victim" {
		t.Errorf("spans[0].TraceID=%q, ждали tr-victim", exp.Spans[0].TraceID)
	}
	if got := exp.Counts["spans"]; got.Returned != 1 || got.Total != 1 {
		t.Errorf("Counts[spans]=%+v, ждали {Returned:1 Total:1}", got)
	}
}

func seedManySpans(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, traceID string, n int, ts time.Time) {
	t.Helper()
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO spans (project_id, trace_id, timestamp)")
	if err != nil {
		t.Fatalf("prepare batch spans: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := batch.Append(uint64(projectID), traceID, ts.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatalf("append batch spans: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send batch spans: %v", err)
	}
}

// Симметрично TestExportSubjectTruncation, но для спанов — у exportSpansByTraceIDs
// свой отдельный цикл по батчам, и без этого теста его усечение не исполнялось вовсе.
func TestExportSubjectSpansTruncation(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p = int64(750)
	const total = 10050
	ts := time.Now().UTC()

	seedTransactionTrace(t, ctx, conn, p, "victim", "tr-victim", ts)
	seedManySpans(t, ctx, conn, p, "tr-victim", total, ts)

	pg := telemetry.NewPurger(conn)
	exp, err := pg.ExportSubject(ctx, p, telemetry.Subject{UserID: "victim"})
	if err != nil {
		t.Fatalf("ExportSubject: %v", err)
	}

	if len(exp.Spans) != 10000 {
		t.Fatalf("spans: получили %d, ждали ровно потолок 10000", len(exp.Spans))
	}
	counts := exp.Counts["spans"]
	if counts.Returned != 10000 || counts.Total != total {
		t.Fatalf("Counts[spans]=%+v, ждали {Returned:10000 Total:%d}", counts, total)
	}
	if !exp.Truncated {
		t.Errorf("exp.Truncated=false при отданных 10000 спанов из %d — молчаливое усечение", total)
	}
}

func seedManyEvents(t *testing.T, ctx context.Context, conn driver.Conn, projectID int64, userID string, n int, ts time.Time) {
	t.Helper()
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO events (event_id, project_id, timestamp, user_id)")
	if err != nil {
		t.Fatalf("prepare batch events: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := batch.Append(uuid.New(), uint64(projectID), ts.Add(time.Duration(i)*time.Millisecond), userID); err != nil {
			t.Fatalf("append batch events: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send batch events: %v", err)
	}
}

// Выше exportRowLimit выгрузка обязана честно назвать «отдано/всего», а не молча
// обрезать и выдать усечённые 10 000 строк за полный ответ субъекту.
func TestExportSubjectTruncation(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const p = int64(800)
	const total = 10050
	ts := time.Now().UTC()
	seedManyEvents(t, ctx, conn, p, "victim", total, ts)

	pg := telemetry.NewPurger(conn)
	exp, err := pg.ExportSubject(ctx, p, telemetry.Subject{UserID: "victim"})
	if err != nil {
		t.Fatalf("ExportSubject: %v", err)
	}

	if len(exp.Events) != 10000 {
		t.Fatalf("events: получили %d, ждали ровно потолок 10000", len(exp.Events))
	}
	counts := exp.Counts["events"]
	if counts.Returned != 10000 || counts.Total != total {
		t.Fatalf("Counts[events]=%+v, ждали {Returned:10000 Total:%d} — субъект должен узнать реальный объём", counts, total)
	}
	if !exp.Truncated {
		t.Errorf("exp.Truncated=false при отданных 10000 из %d — молчаливое усечение", total)
	}

	// Ниже потолка — выгрузка полна, признака усечения нет.
	exp2, err := pg.ExportSubject(ctx, p, telemetry.Subject{UserID: "does-not-exist"})
	if err != nil {
		t.Fatalf("ExportSubject (пустой субъект): %v", err)
	}
	if exp2.Truncated {
		t.Errorf("exp2.Truncated=true без данных субъекта — ложное усечение")
	}
	if got := exp2.Counts["events"]; got.Returned != 0 || got.Total != 0 {
		t.Errorf("Counts[events]=%+v для несуществующего субъекта, ждали {0 0}", got)
	}
}

// IP-only субъект не даёт условий ни для одной таблицы кроме events — единственный
// источник остальных ключей в Counts — предзаполнение по subjectTables, а не recordCount.
func TestExportSubjectCountsFullKeysIPOnly(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := telemetry.NewPurger(conn)
	exp, err := p.ExportSubject(ctx, 850, telemetry.Subject{IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("ExportSubject by IP: %v", err)
	}

	want := []string{"events", "transactions", "spans", "metric_points", "logs"}
	if len(exp.Counts) != len(want) {
		t.Fatalf("Counts содержит %d ключей, ждали %d: %+v", len(exp.Counts), len(want), exp.Counts)
	}
	for _, table := range want {
		if _, ok := exp.Counts[table]; !ok {
			t.Errorf("Counts не содержит %q для субъекта, заданного только IP — таблица выпала из единого перечня", table)
		}
	}
}

var errAbortedCursor = errors.New("telemetry_test: simulated ClickHouse cursor abort")

// После keep успешных Next() имитирует штатный конец (false), но Err() отдаёт
// errAbortedCursor, а Close() — nil, как в реальном обрыве соединения clickhouse-go.
type abortingRows struct {
	driver.Rows
	keep int
}

func (r *abortingRows) Next() bool {
	if r.keep <= 0 {
		return false
	}
	r.keep--
	return r.Rows.Next()
}

func (r *abortingRows) Err() error { return errAbortedCursor }

func (r *abortingRows) Close() error {
	_ = r.Rows.Close()
	return nil
}

// Подменяет результат failAt-го по счёту вызова Query на abortingRows, остальные
// вызовы идут через настоящий Rows без изменений.
type countingConn struct {
	driver.Conn
	n        int
	failAt   int
	keepRows int
}

func (c *countingConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	rows, err := c.Conn.Query(ctx, query, args...)
	c.n++
	if err != nil || c.n != c.failAt {
		return rows, err
	}
	return &abortingRows{Rows: rows, keep: c.keepRows}, nil
}

func TestExportSubjectRowsErrSurfaces(t *testing.T) {
	ctx := context.Background()
	conn := testenv.MigratedCH(t)
	const p = int64(600)
	ts := time.Now().UTC()

	// По 5 строк в каждой таблице — обрыв после keepRows=1 гарантированно
	// застаёт курсор ДО того, как он успел бы отдать все строки штатно.
	for i := 0; i < 5; i++ {
		seedEvents(t, ctx, conn, p, "victim", "1.2.3.4", "victim@x.com", ts)
		seedTransactions(t, ctx, conn, p, "victim", ts)
		seedMetricPointAttr(t, ctx, conn, p, map[string]string{"user.id": "victim"}, ts)
		seedLogAttr(t, ctx, conn, p, map[string]string{"user.id": "victim"}, ts)
	}

	sub := telemetry.Subject{UserID: "victim", Email: "victim@x.com"}

	tests := []struct {
		name string
		k    int
	}{
		{"events", 1},
		{"trace_ids", 2},
		{"spans", 3},
		{"transactions", 4},
		{"metric_points", 5},
		{"logs", 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := &countingConn{Conn: conn, failAt: tt.k, keepRows: 1}
			purger := telemetry.NewPurger(cc)

			if _, err := purger.ExportSubject(ctx, p, sub); !errors.Is(err, errAbortedCursor) {
				t.Fatalf("ExportSubject: err=%v, want обёрнутую errAbortedCursor — обрыв курсора %s должен всплыть, а не отдать усечённую выгрузку или другую ошибку", err, tt.name)
			}
		})
	}
}
