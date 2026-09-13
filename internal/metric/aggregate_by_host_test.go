package metric

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedHistogramHost(t *testing.T, conn interface {
	Exec(ctx context.Context, query string, args ...any) error
}, projectID int64, name, env, host string, ts time.Time, count uint64, bc []uint64, eb []float64) {
	t.Helper()
	if err := conn.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, host, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'histogram', 'ms', 'api', ?, ?, map(), ?, 0, ?, ?, ?, 0, 'cumulative')`,
		projectID, name, env, host, ts, count, bc, eb); err != nil {
		t.Fatalf("seed histogram host: %v", err)
	}
}

// Прогоняет старый путь (Aggregate по каждому хосту отдельно) — эталон, с которым сверяется
// AggregateByHost. Отсутствие ok=true в результате — отсутствие ключа в карте, не ноль.
func aggregateOldWay(t *testing.T, q *Query, pid int64, name, env string, hosts []string,
	matchers []LabelMatcher, agg string, from, to time.Time) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, h := range hosts {
		v, ok, err := q.Aggregate(context.Background(), pid, name, env, h, matchers, agg, from, to)
		if err != nil {
			t.Fatalf("Aggregate(%s): %v", h, err)
		}
		if ok {
			out[h] = v
		}
	}
	return out
}

// Точное совпадение — гарантировано только на этом маленьком корпусе (единицы точек на хост, целые
// значения); avg на плотных рядах может разойтись в младших разрядах, см. aggregateScalarByHost.
func assertMapsEqual(t *testing.T, got, want map[string]float64, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d hosts %+v, want %d hosts %+v", label, len(got), got, len(want), want)
	}
	for h, wv := range want {
		gv, ok := got[h]
		if !ok {
			t.Fatalf("%s: host %q missing from batched result, want %v", label, h, wv)
		}
		if gv != wv {
			t.Fatalf("%s: host %q = %v, want %v (exact match expected on this corpus)", label, h, gv, wv)
		}
	}
}

// Scalar-ветка (gauge, max/avg/last): несколько хостов, один без данных вовсе в окне,
// матчер по атрибуту сохраняется в батче так же, как в точечном Aggregate.
func TestAggregateByHostScalarMatchesLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 9001

	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-1", now.Add(-4*time.Minute), 10, nil)
	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-1", now.Add(-1*time.Minute), 90, nil)
	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-2", now.Add(-2*time.Minute), 40, nil)
	// web-3: точки за пределами окна — должен быть исключён из обеих карт.
	seedGaugeHost(t, conn, pid, "system.filesystem.utilization", "prod", "web-3", now.Add(-2*time.Hour), 99, nil)

	hosts := []string{"web-1", "web-2", "web-3"}
	from, to := now.Add(-5*time.Minute), now

	for _, agg := range []string{"max", "avg", "last"} {
		old := aggregateOldWay(t, q, pid, "system.filesystem.utilization", "prod", hosts, nil, agg, from, to)
		batched, err := q.AggregateByHost(context.Background(), pid, "system.filesystem.utilization", "prod", nil, agg, from, to)
		if err != nil {
			t.Fatalf("AggregateByHost(%s): %v", agg, err)
		}
		assertMapsEqual(t, batched, old, "agg="+agg)
	}

	// matchers: только state=used должен пройти, отдельный gauge того же имени/окна с state=free — не должен.
	seedGaugeHost(t, conn, pid, "system.memory.utilization", "prod", "web-1", now.Add(-1*time.Minute), 70,
		map[string]string{"state": "used"})
	seedGaugeHost(t, conn, pid, "system.memory.utilization", "prod", "web-1", now.Add(-1*time.Minute), 30,
		map[string]string{"state": "free"})
	seedGaugeHost(t, conn, pid, "system.memory.utilization", "prod", "web-2", now.Add(-1*time.Minute), 55,
		map[string]string{"state": "used"})
	matchers := []LabelMatcher{{Key: "state", Value: "used"}}
	old := aggregateOldWay(t, q, pid, "system.memory.utilization", "prod", hosts, matchers, "avg", from, to)
	batched, err := q.AggregateByHost(context.Background(), pid, "system.memory.utilization", "prod", matchers, "avg", from, to)
	if err != nil {
		t.Fatalf("AggregateByHost matchers: %v", err)
	}
	assertMapsEqual(t, batched, old, "matchers state=used")
	if _, ok := batched["web-2"]; !ok || len(batched) != 2 {
		t.Fatalf("matchers state=used: got %+v, want exactly web-1 and web-2", batched)
	}
}

// Rate-ветка (sum monotonic cumulative): один хост со сбросом счётчика, другой — монотонный рост,
// третий — единственная точка (rateSeries требует минимум 2 бакета, должен отсутствовать в обеих картах).
func TestAggregateByHostRateMatchesLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 9002
	const name = "http.requests.total"

	base := now.Add(-5 * time.Minute)
	for i := 0; i <= 5; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		seedSumCumulativeHost(t, conn, pid, name, "web-1", ts, float64(100*i), nil)
	}
	// web-2: рост, потом сброс счётчика на середине (delta<0 -> 0 для этого перехода).
	resetVals := []float64{0, 100, 200, 20, 120, 220}
	for i, v := range resetVals {
		ts := base.Add(time.Duration(i) * time.Minute)
		seedSumCumulativeHost(t, conn, pid, name, "web-2", ts, v, nil)
	}
	// web-3: одна точка — нет ряда, должен отсутствовать в обеих картах.
	seedSumCumulativeHost(t, conn, pid, name, "web-3", now.Add(-1*time.Minute), 500, nil)

	hosts := []string{"web-1", "web-2", "web-3"}
	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)

	for _, agg := range []string{"max", "avg", "min"} {
		old := aggregateOldWay(t, q, pid, name, "", hosts, nil, agg, from, to)
		batched, err := q.AggregateByHost(context.Background(), pid, name, "", nil, agg, from, to)
		if err != nil {
			t.Fatalf("AggregateByHost rate(%s): %v", agg, err)
		}
		assertMapsEqual(t, batched, old, "rate agg="+agg)
	}
	batched, err := q.AggregateByHost(context.Background(), pid, name, "", nil, "max", from, to)
	if err != nil {
		t.Fatalf("AggregateByHost rate: %v", err)
	}
	if _, ok := batched["web-3"]; ok {
		t.Fatalf("web-3 (single point) must be absent from rate result, got %v", batched["web-3"])
	}
}

// Increase-ветка (agg=increase на sum monotonic cumulative): сумма положительных приростов за окно,
// сброс счётчика обнуляет только свой переход — как aggregateIncrease на одиночном хосте.
func TestAggregateByHostIncreaseMatchesLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 9003
	const name = "http.requests.total"

	base := now.Add(-5 * time.Minute)
	for i := 0; i <= 5; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		seedSumCumulativeHost(t, conn, pid, name, "web-1", ts, float64(50*i), nil)
	}
	resetVals := []float64{0, 100, 200, 20, 120, 220}
	for i, v := range resetVals {
		ts := base.Add(time.Duration(i) * time.Minute)
		seedSumCumulativeHost(t, conn, pid, name, "web-2", ts, v, nil)
	}
	// web-3: одна точка — aggregateIncrease требует cnt>=2, должен отсутствовать в обеих картах
	// (без этого хоста мутация, снимающая отсечение по cnt<2, ничего не роняет).
	seedSumCumulativeHost(t, conn, pid, name, "web-3", now.Add(-1*time.Minute), 500, nil)

	hosts := []string{"web-1", "web-2", "web-3"}
	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)

	old := aggregateOldWay(t, q, pid, name, "", hosts, nil, "increase", from, to)
	batched, err := q.AggregateByHost(context.Background(), pid, name, "", nil, "increase", from, to)
	if err != nil {
		t.Fatalf("AggregateByHost increase: %v", err)
	}
	assertMapsEqual(t, batched, old, "increase")
	if len(old) != 2 {
		t.Fatalf("expected exactly web-1 and web-2 to have an increase value, got %+v", old)
	}
	if _, ok := batched["web-3"]; ok {
		t.Fatalf("web-3 (single point) must be absent from increase result, got %v", batched["web-3"])
	}
}

// Histogram-ветка (percentile): sum(count)==0 для хоста должен исключить его из карты, как cnt==0
// в Aggregate — не просто отсутствие строк, а нулевая сумма count при наличии строки.
func TestAggregateByHostHistogramMatchesLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 9004
	const name = "http.duration"

	seedHistogramHost(t, conn, pid, name, "prod", "web-1", now.Add(-time.Minute), 12, []uint64{2, 8, 2}, []float64{100, 500})
	seedHistogramHost(t, conn, pid, name, "prod", "web-2", now.Add(-time.Minute), 20, []uint64{5, 10, 5}, []float64{100, 500})
	// web-3: строка есть, но count==0 — должен остаться исключённым, как в Aggregate.
	seedHistogramHost(t, conn, pid, name, "prod", "web-3", now.Add(-time.Minute), 0, []uint64{0, 0, 0}, []float64{100, 500})

	hosts := []string{"web-1", "web-2", "web-3"}
	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)

	for _, agg := range []string{"p50", "p95", "p99"} {
		old := aggregateOldWay(t, q, pid, name, "prod", hosts, nil, agg, from, to)
		batched, err := q.AggregateByHost(context.Background(), pid, name, "prod", nil, agg, from, to)
		if err != nil {
			t.Fatalf("AggregateByHost histogram(%s): %v", agg, err)
		}
		assertMapsEqual(t, batched, old, "histogram agg="+agg)
	}
	batched, err := q.AggregateByHost(context.Background(), pid, name, "prod", nil, "p95", from, to)
	if err != nil {
		t.Fatalf("AggregateByHost histogram: %v", err)
	}
	if _, ok := batched["web-3"]; ok {
		t.Fatalf("web-3 (count=0) must be absent from histogram result, got %v", batched["web-3"])
	}
}

// delta-temporality sum (monotonic=0): уходит в scalarAggExpr, а не в rate/increase —
// agg=increase на delta-sum суммирует значения как есть (sum(value)), как в Aggregate.
func TestAggregateByHostDeltaSumMatchesLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 9005
	const name = "jobs.processed"

	seedSumDeltaHost(t, conn, pid, name, "web-1", now.Add(-3*time.Minute), 10)
	seedSumDeltaHost(t, conn, pid, name, "web-1", now.Add(-1*time.Minute), 15)
	seedSumDeltaHost(t, conn, pid, name, "web-2", now.Add(-2*time.Minute), 7)

	hosts := []string{"web-1", "web-2"}
	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)

	for _, agg := range []string{"increase", "sum", "max"} {
		old := aggregateOldWay(t, q, pid, name, "", hosts, nil, agg, from, to)
		batched, err := q.AggregateByHost(context.Background(), pid, name, "", nil, agg, from, to)
		if err != nil {
			t.Fatalf("AggregateByHost delta-sum(%s): %v", agg, err)
		}
		assertMapsEqual(t, batched, old, "delta-sum agg="+agg)
	}
}

func seedSumDeltaHost(t *testing.T, conn interface {
	Exec(ctx context.Context, query string, args ...any) error
}, projectID int64, name, host string, ts time.Time, val float64) {
	t.Helper()
	if err := conn.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, host, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'sum', '1', 'api', '', ?, map(), ?, ?, 0, [], [], 0, 'delta')`,
		projectID, name, host, ts, val); err != nil {
		t.Fatalf("seed sum delta host: %v", err)
	}
}

// Пустое окно: обе карты должны быть пустыми, а не содержать нулевые значения по отсутствующим хостам.
func TestAggregateByHostEmptyWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	now := time.Now().UTC().Truncate(time.Minute)
	const pid = 9006

	seedGaugeHost(t, conn, pid, "cpu.idle", "prod", "web-1", now.Add(-2*time.Hour), 42, nil)

	from, to := now.Add(-5*time.Minute), now

	batched, err := q.AggregateByHost(context.Background(), pid, "cpu.idle", "prod", nil, "avg", from, to)
	if err != nil {
		t.Fatalf("AggregateByHost empty window: %v", err)
	}
	if len(batched) != 0 {
		t.Fatalf("empty window: got %+v, want empty map", batched)
	}
}
