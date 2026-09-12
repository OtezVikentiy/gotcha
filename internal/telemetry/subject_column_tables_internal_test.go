package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

type execCapturingConn struct {
	driver.Conn
	execs []string
}

func (c *execCapturingConn) Exec(ctx context.Context, query string, args ...any) error {
	c.execs = append(c.execs, query)
	return c.Conn.Exec(ctx, query, args...)
}

// Имя, вписанное в subjectColumnTables без соответствующего ALTER ... DELETE в
// PurgeSubject, обязано ронять этот тест — список не только про признание в схеме.
func TestSubjectColumnTablesActuallyPurged(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cc := &execCapturingConn{Conn: conn}
	p := NewPurger(cc)
	if _, err := p.PurgeSubject(ctx, 950, Subject{Email: "a@b.com", UserID: "victim"}); err != nil {
		t.Fatalf("PurgeSubject: %v", err)
	}

	for _, table := range subjectColumnTables {
		want := "ALTER TABLE " + table + " DELETE"
		touched := false
		for _, q := range cc.execs {
			if strings.Contains(q, want) {
				touched = true
				break
			}
		}
		if !touched {
			t.Errorf("subjectColumnTables содержит %q, но PurgeSubject не выполнил по ней %q — список не несёт стирания", table, want)
		}
	}
}
