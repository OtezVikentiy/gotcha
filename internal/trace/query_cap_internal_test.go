package trace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Часовой: Query обязан вернуть именно эту ошибку — иначе перехват не сработал и
// проверка текста запроса ничего не доказывает.
var errQueryCapture = errors.New("query capture")

// Встроенный nil-интерфейс даёт остальные методы driver.Conn — их вызов запаникует,
// что и нужно: тест обязан падать, а не молча проходить, если код пойдёт другим путём.
type captureConn struct {
	driver.Conn
	query string
	args  []any
}

func (c *captureConn) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	c.query = query
	c.args = args
	return nil, errQueryCapture
}

func TestEndpointsQueryCapsRowsAndExecutionTime(t *testing.T) {
	conn := &captureConn{}
	q := NewQuery(conn)
	from := time.Now().Add(-time.Hour)
	to := time.Now()

	if _, _, err := q.Endpoints(context.Background(), 1, from, to, "", 0); !errors.Is(err, errQueryCapture) {
		t.Fatalf("Endpoints error = %v, want обёрнутую errQueryCapture (перехват не сработал)", err)
	}
	if !strings.Contains(conn.query, "LIMIT ?") {
		t.Errorf("запрос Endpoints не несёт LIMIT: %s", conn.query)
	}
	if !strings.Contains(conn.query, "SETTINGS max_execution_time = 10") {
		t.Errorf("запрос Endpoints не несёт max_execution_time: %s", conn.query)
	}
	if len(conn.args) == 0 || conn.args[len(conn.args)-1] != endpointsRowCap+1 {
		t.Fatalf("последний аргумент запроса = %v, want потолок строк %d", conn.args, endpointsRowCap+1)
	}
}

// fakeEndpointRows реализует driver.Rows и реально сканируется — в отличие от captureConn,
// который падает раньше Scan. Только так проверяется усечение len(out) в Endpoints, а не
// только форма запроса.
type fakeEndpointRows struct {
	n, i int
}

func (r *fakeEndpointRows) Next() bool {
	if r.i >= r.n {
		return false
	}
	r.i++
	return true
}

func (r *fakeEndpointRows) Scan(dest ...any) error {
	*(dest[0].(*string)) = fmt.Sprintf("tx-%d", r.i)
	*(dest[1].(*uint64)) = 1
	*(dest[2].(*uint64)) = 0
	*(dest[3].(*[]float64)) = []float64{1, 2, 3, 4}
	*(dest[4].(*[]string)) = []string{"production"}
	return nil
}

func (r *fakeEndpointRows) ScanStruct(any) error             { return nil }
func (r *fakeEndpointRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *fakeEndpointRows) Totals(dest ...any) error         { return nil }
func (r *fakeEndpointRows) Columns() []string                { return nil }
func (r *fakeEndpointRows) Close() error                     { return nil }
func (r *fakeEndpointRows) Err() error                       { return nil }
func (r *fakeEndpointRows) HasData() bool                    { return r.i < r.n }

type rowsConn struct {
	driver.Conn
	rows driver.Rows
}

func (c *rowsConn) Query(_ context.Context, _ string, _ ...any) (driver.Rows, error) {
	return c.rows, nil
}

func TestEndpointsTruncatesAtRowCap(t *testing.T) {
	conn := &rowsConn{rows: &fakeEndpointRows{n: endpointsRowCap + 1}}
	q := NewQuery(conn)

	out, truncated, err := q.Endpoints(context.Background(), 1, time.Now().Add(-time.Hour), time.Now(), "", 0)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if !truncated {
		t.Fatal("truncated = false, want true — ClickHouse вернул cap+1 строк")
	}
	if len(out) != endpointsRowCap {
		t.Fatalf("len(out) = %d, want ровно потолок %d", len(out), endpointsRowCap)
	}
}

func TestEndpointsNotTruncatedAtExactCap(t *testing.T) {
	conn := &rowsConn{rows: &fakeEndpointRows{n: endpointsRowCap}}
	q := NewQuery(conn)

	out, truncated, err := q.Endpoints(context.Background(), 1, time.Now().Add(-time.Hour), time.Now(), "", 0)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if truncated {
		t.Fatal("truncated = true при len(out) == cap, want false — потолок не должен ложно срабатывать на границе")
	}
	if len(out) != endpointsRowCap {
		t.Fatalf("len(out) = %d, want %d", len(out), endpointsRowCap)
	}
}

func TestApdexByTransactionScopesToTransactionsAndCapsExecutionTime(t *testing.T) {
	conn := &captureConn{}
	q := NewQuery(conn)
	from := time.Now().Add(-time.Hour)
	to := time.Now()

	if _, err := q.apdexByTransaction(context.Background(), 1, from, to, "", 300, []string{"GET /a", "GET /b"}); !errors.Is(err, errQueryCapture) {
		t.Fatalf("apdexByTransaction error = %v, want обёрнутую errQueryCapture", err)
	}
	if !strings.Contains(conn.query, "transaction IN ?") {
		t.Errorf("запрос apdex не сужен по списку transaction: %s", conn.query)
	}
	if !strings.Contains(conn.query, "SETTINGS max_execution_time = 10") {
		t.Errorf("запрос apdex не несёт max_execution_time: %s", conn.query)
	}
}

func TestApdexByTransactionEmptyTransactionsSkipsQuery(t *testing.T) {
	conn := &captureConn{}
	q := NewQuery(conn)

	out, err := q.apdexByTransaction(context.Background(), 1, time.Now(), time.Now(), "", 300, nil)
	if err != nil {
		t.Fatalf("apdexByTransaction(транзакции=nil): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("apdexByTransaction(транзакции=nil) = %v, want пустую карту", out)
	}
	if conn.query != "" {
		t.Errorf("при пустом списке transaction запрос не должен уходить в ClickHouse: %s", conn.query)
	}
}
