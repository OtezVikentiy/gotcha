package telemetry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestExportSubject проверяет, что экспорт (право субъекта на доступ, 152-ФЗ)
// возвращает ТОЛЬКО строки субъекта в рамках проекта: и по email, и по user_id,
// и не тянет данные чужого субъекта или чужого проекта.
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

	// Экспорт по email: одно событие в p2 и ни одной транзакции (email там нет).
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

	// Экспорт по user_id: событие + транзакция субъекта victim в p2.
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

	// Пустой субъект — ошибка, ничего не экспортируем.
	if _, err := p.ExportSubject(ctx, p2, telemetry.Subject{}); err == nil {
		t.Errorf("ExportSubject с пустым субъектом должен вернуть ошибку")
	}
}

// TestExportSubjectTransactionTags проверяет паритет выгрузки с чисткой: субъект,
// заданный email, ДОЛЖЕН видеть свои транзакции, где email лежит в тегах (OTLP),
// а не только в колонке user_id. Чужие транзакции в выгрузку не попадают.
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

	// Экспорт по email тянет обе транзакции субъекта из тегов, чужую — нет.
	exp, err := p2.ExportSubject(ctx, p, telemetry.Subject{Email: "a@b.com"})
	if err != nil {
		t.Fatalf("ExportSubject by email: %v", err)
	}
	if len(exp.Transactions) != 2 {
		t.Errorf("transactions по email в тегах: получили %d, ждали 2", len(exp.Transactions))
	}
}

// TestExportSubjectMetricPoints проверяет паритет выгрузки с чисткой (152-ФЗ):
// ПДн субъекта из metric_points.attributes попадают в экспорт по user_id, но не
// по IP-only субъекту (метрики по IP не сегментируются).
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

	// Экспорт по user_id: только метрика субъекта victim, чужая не тянется.
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

	// Экспорт по IP-only субъекту: метрик нет (attributes не содержат IP).
	expIP, err := p.ExportSubject(ctx, p2, telemetry.Subject{IP: "192.168.0.1"})
	if err != nil {
		t.Fatalf("ExportSubject by IP: %v", err)
	}
	if len(expIP.MetricPoints) != 0 {
		t.Errorf("metric_points по IP: получили %d, ждали 0 (метрики не сегментируются по IP)", len(expIP.MetricPoints))
	}
}

// TestExportSubjectLogs проверяет паритет выгрузки с чисткой (152-ФЗ): ПДн
// субъекта из logs.log_attributes попадают в экспорт по всем четырём ключам
// (user.id/enduser.id ← UserID, user.email/enduser.email ← Email), не отдавая
// логи постороннего субъекта или чужого проекта.
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

	// Экспорт по IP-only субъекту: логов нет (log_attributes не содержат IP).
	expIP, err := p.ExportSubject(ctx, p2, telemetry.Subject{IP: "192.168.0.1"})
	if err != nil {
		t.Fatalf("ExportSubject by IP: %v", err)
	}
	if len(expIP.Logs) != 0 {
		t.Errorf("logs по IP: получили %d, ждали 0 (логи не сегментируются по IP)", len(expIP.Logs))
	}
}

// errAbortedCursor — синтетическая ошибка обрыва курсора, которую видит
// только Rows.Err() (см. abortingRows ниже).
var errAbortedCursor = errors.New("telemetry_test: simulated ClickHouse cursor abort")

// abortingRows оборачивает реальный driver.Rows и обрывает курсор ПОСЛЕ
// keep успешных Next(): дальше Next() возвращает false, как будто строки
// кончились штатно, Err() отдаёт errAbortedCursor, а Close() — как
// настоящий обрыв соединения в clickhouse-go (K4-3, аудит перед 1.0) —
// свою ошибку не поднимает, отдавая nil независимо от исхода реального
// Close(). Обрыв синтетический (без разрыва настоящего соединения или
// отмены контекста), поэтому воспроизводится детерминированно и не задевает
// остальные вызовы Query в том же ExportSubject.
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

// countingConn оборачивает реальное соединение и подменяет результат
// failAt-го по счёту вызова Query на abortingRows — курсор ИМЕННО этого
// вызова обрывается после keepRows строк, остальные вызовы Query того же
// ExportSubject идут через настоящий Rows без изменений.
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

// TestExportSubjectRowsErrSurfaces проверяет, что обрыв курсора ClickHouse
// ПОСЛЕ успешного Query — ошибка выгрузки субъекта, а не тихо усечённый
// результат (K4-3). Четыре подтеста k=1..4 обрывают курсор events/
// transactions/metric_points/logs соответственно (остальные три вызова
// Query идут штатно, без единой строки потерь) — каждый доказывает, что
// СВОЙ цикл SubjectExport проверяет rows.Err(), а не только rows.Close(),
// которое (см. abortingRows) обрыв не показывает.
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
		{"transactions", 2},
		{"metric_points", 3},
		{"logs", 4},
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
