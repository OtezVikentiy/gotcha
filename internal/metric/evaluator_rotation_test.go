package metric_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

// projectStuckCH виснет на каждом запросе и пишет project_id (первый аргумент) —
// так видно, какое правило реально дошло до ClickHouse за тик.
type projectStuckCH struct {
	driver.Conn
	mu       sync.Mutex
	projects []int64
}

func (c *projectStuckCH) record(args []any) {
	if len(args) == 0 {
		return
	}
	pid, ok := args[0].(int64)
	if !ok {
		return
	}
	c.mu.Lock()
	c.projects = append(c.projects, pid)
	c.mu.Unlock()
}

func (c *projectStuckCH) QueryRow(ctx context.Context, _ string, args ...any) driver.Row {
	c.record(args)
	<-ctx.Done()
	return stuckRow{err: ctx.Err()}
}

func (c *projectStuckCH) Query(ctx context.Context, _ string, args ...any) (driver.Rows, error) {
	c.record(args)
	<-ctx.Done()
	return nil, ctx.Err()
}

type stuckRow struct{ err error }

func (r stuckRow) Err() error           { return r.err }
func (r stuckRow) Scan(...any) error    { return r.err }
func (r stuckRow) ScanStruct(any) error { return r.err }

func (c *projectStuckCH) touchedProjects() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.projects...)
}

// Без ротации бюджет тика (пол 10с) всегда обрывается на одном и том же первом правиле.
func TestEvaluatorRotatesAcrossTicks(t *testing.T) {
	rules := &fakeRuleLister{rules: []metric.Rule{
		{ID: 1, ProjectID: 201, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 100, WindowSeconds: 300, Enabled: true},
		{ID: 2, ProjectID: 202, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 100, WindowSeconds: 300, Enabled: true},
		{ID: 3, ProjectID: 203, MetricName: "cpu", Aggregation: "avg", Comparator: "gt", Threshold: 100, WindowSeconds: 300, Enabled: true},
	}}
	stuck := &projectStuckCH{}
	eval := &metric.Evaluator{
		Rules:    rules,
		Query:    metric.NewQuery(stuck),
		Interval: time.Second, // бюджет: пол minTickBudget = 10с
	}

	tickAndFirstTouched := func(tickNo int) int64 {
		before := len(stuck.touchedProjects())
		eval.Tick(context.Background())
		got := stuck.touchedProjects()[before:]
		if len(got) == 0 {
			t.Fatalf("тик %d: ClickHouse ни разу не запрошен — тест не проверяет то, что должен", tickNo)
		}
		first := got[0]
		for _, pid := range got {
			if pid != first {
				t.Fatalf("тик %d: за один тик задето не одно правило: %v", tickNo, got)
			}
		}
		return first
	}

	// За 4 тика курсор обязан пройти все три правила по кругу и вернуться к первому.
	want := []int64{201, 202, 203, 201}
	for i, w := range want {
		got := tickAndFirstTouched(i + 1)
		if got != w {
			t.Fatalf("тик %d: обработано правило проекта %d, want %d", i+1, got, w)
		}
		if skipped := eval.LastTickSkippedRules(); skipped != 2 {
			t.Errorf("тик %d: LastTickSkippedRules() = %d, want 2 (два правила из трёх не влезли в бюджет)", i+1, skipped)
		}
	}
}
