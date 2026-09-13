package metric

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedSumCumulative(t *testing.T, conn interface {
	Exec(ctx context.Context, query string, args ...any) error
}, projectID int64, name, env string, ts time.Time, val float64) {
	t.Helper()
	if err := conn.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'sum', '1', 'api', ?, map(), ?, ?, 0, [], [], 1, 'cumulative')`,
		projectID, name, env, ts, val); err != nil {
		t.Fatalf("seed sum cumulative: %v", err)
	}
}

// delta-temporality sum: значения уже точечные приращения, не бегущий счётчик —
// monotonic=0 уводит с пути rateSeries/aggregateIncrease в scalarAggExpr.
func seedSumDelta(t *testing.T, conn interface {
	Exec(ctx context.Context, query string, args ...any) error
}, projectID int64, name, env string, ts time.Time, val float64) {
	t.Helper()
	if err := conn.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'sum', '1', 'api', ?, map(), ?, ?, 0, [], [], 0, 'delta')`,
		projectID, name, env, ts, val); err != nil {
		t.Fatalf("seed sum delta: %v", err)
	}
}

func seedHistogram(t *testing.T, conn interface {
	Exec(ctx context.Context, query string, args ...any) error
}, projectID int64, name, env string, ts time.Time, count uint64, bc []uint64, eb []float64) {
	t.Helper()
	if err := conn.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'histogram', 'ms', 'api', ?, map(), ?, 0, ?, ?, ?, 0, 'cumulative')`,
		projectID, name, env, ts, count, bc, eb); err != nil {
		t.Fatalf("seed histogram: %v", err)
	}
}

func TestQueryRateSeries(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 71
	seedSumCumulative(t, conn, pid, "req.total", "prod", now.Add(-2*time.Minute), 100)
	seedSumCumulative(t, conn, pid, "req.total", "prod", now.Add(-1*time.Minute), 160)

	pts, err := q.Series(ctx, pid, "req.total", "prod", "", nil, "avg",
		now.Add(-10*time.Minute), now.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("rate Series: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("rate points = %+v, want 1 (delta of 2 buckets)", pts)
	}
	// 60 приращения за 60 секунд = 1/s.
	if pts[0].V < 0.99 || pts[0].V > 1.01 {
		t.Fatalf("rate = %v, want ≈1/s", pts[0].V)
	}
}

func TestQueryRateSeriesSingleBucket(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 72
	seedSumCumulative(t, conn, pid, "req.one", "prod", now.Add(-30*time.Second), 100)

	pts, err := q.Series(ctx, pid, "req.one", "prod", "", nil, "avg",
		now.Add(-10*time.Minute), now.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("rate Series single: %v", err)
	}
	if len(pts) != 0 {
		t.Fatalf("single-bucket rate points = %+v, want 0", pts)
	}
}

func TestAggregateHistogramPercentile(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 73
	seedHistogram(t, conn, pid, "http.dur", "prod", now.Add(-time.Minute), 12, []uint64{2, 8, 2}, []float64{100, 500})

	v, ok, err := q.Aggregate(ctx, pid, "http.dur", "prod", "", nil, "p95",
		now.Add(-10*time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Aggregate histogram: %v", err)
	}
	if !ok {
		t.Fatal("Aggregate histogram: ok=false, want true (has data)")
	}
	if v < 100 {
		t.Fatalf("p95 = %v, want >= 100", v)
	}
}

func TestAggregateNoData(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 74
	// Точка есть, но давно — попадёт в metricType, но не в окно запроса.
	seedGauge(t, conn, pid, "cpu", "prod", now.Add(-2*time.Hour), 42, nil)

	v, ok, err := q.Aggregate(ctx, pid, "cpu", "prod", "", nil, "avg",
		now.Add(-5*time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Aggregate no-data: %v", err)
	}
	if ok {
		t.Fatalf("Aggregate no-data: ok=true (v=%v), want false", v)
	}
}

func TestQueryRateSeriesSparseScrape(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 7101
	// Скрейп раз в 5 минут, счётчик растёт на 300 → истинная скорость 1/с.
	seedSumCumulative(t, conn, pid, "sparse.total", "prod", now.Add(-10*time.Minute), 0)
	seedSumCumulative(t, conn, pid, "sparse.total", "prod", now.Add(-5*time.Minute), 300)

	// Шаг корзины — минута, то есть ВПЯТЕРО меньше интервала скрейпа.
	pts, err := q.Series(ctx, pid, "sparse.total", "prod", "", nil, "avg",
		now.Add(-30*time.Minute), now.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("точек = %d, want 1: %+v", len(pts), pts)
	}
	if pts[0].V < 0.99 || pts[0].V > 1.01 {
		t.Fatalf("скорость = %v, want ≈1/с (деление на ширину корзины дало бы ≈5/с)", pts[0].V)
	}
}

func TestAggregateMatchesSeriesForCumulativeCounter(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 7102
	// Счётчик уже намотал миллион и растёт на 60 в минуту → скорость 1/с.
	seedSumCumulative(t, conn, pid, "huge.total", "prod", now.Add(-3*time.Minute), 1_000_000)
	seedSumCumulative(t, conn, pid, "huge.total", "prod", now.Add(-2*time.Minute), 1_000_060)
	seedSumCumulative(t, conn, pid, "huge.total", "prod", now.Add(-1*time.Minute), 1_000_120)

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)
	got, ok, err := q.Aggregate(ctx, pid, "huge.total", "prod", "", nil, "avg", from, to)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if !ok {
		t.Fatal("Aggregate: данных нет, ожидались")
	}
	if got > 100 {
		t.Fatalf("Aggregate = %v — это сырое значение счётчика, а график показывает скорость ≈1/с", got)
	}
	if got < 0.5 || got > 2 {
		t.Fatalf("Aggregate = %v, want ≈1/с (как на графике)", got)
	}
}

// Два скрейпа с шагом 30с, прирост счётчика 60: increase обязан вернуть 60
// (честный прирост за окно), а не 2.0 (что дала бы скорость 60/30с — старая
// семантика sum/avg на rate, которую и чинит эта агрегация).
func TestAggregateIncreaseWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 7110
	seedSumCumulative(t, conn, pid, "deadlocks.total", "prod", now.Add(-30*time.Second), 100)
	seedSumCumulative(t, conn, pid, "deadlocks.total", "prod", now, 160)

	got, ok, err := q.Aggregate(ctx, pid, "deadlocks.total", "prod", "", nil, "increase",
		now.Add(-40*time.Second), now.Add(time.Second))
	if err != nil {
		t.Fatalf("Aggregate increase: %v", err)
	}
	if !ok {
		t.Fatal("Aggregate increase: ok=false, want true (has data)")
	}
	if got < 59.9 || got > 60.1 {
		t.Fatalf("increase = %v, want 60 (честный прирост за окно)", got)
	}
}

// Сброс счётчика (экспортёр перезапустился): переход 100→10 обнуляется, как в
// rateSeries, дальше рост 10→40 считается заново — итог 30, не 130 и не -60.
func TestAggregateIncreaseCounterReset(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 7111
	seedSumCumulative(t, conn, pid, "resets.total", "prod", now.Add(-2*time.Minute), 100)
	seedSumCumulative(t, conn, pid, "resets.total", "prod", now.Add(-time.Minute), 10)
	seedSumCumulative(t, conn, pid, "resets.total", "prod", now, 40)

	got, ok, err := q.Aggregate(ctx, pid, "resets.total", "prod", "", nil, "increase",
		now.Add(-10*time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Aggregate increase reset: %v", err)
	}
	if !ok {
		t.Fatal("Aggregate increase reset: ok=false, want true (has data)")
	}
	if got < 29.9 || got > 30.1 {
		t.Fatalf("increase после сброса = %v, want 30 (100→10 обнулился, 10→40 = 30)", got)
	}
}

// Одна точка в окне — прироста считать не из чего, как и у rateSeries.
func TestAggregateIncreaseSinglePointNoData(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 7112
	seedSumCumulative(t, conn, pid, "one.total", "prod", now.Add(-30*time.Second), 100)

	got, ok, err := q.Aggregate(ctx, pid, "one.total", "prod", "", nil, "increase",
		now.Add(-10*time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Aggregate increase single: %v", err)
	}
	if ok {
		t.Fatalf("Aggregate increase single: ok=true (v=%v), want false", got)
	}
}

// Сброс дважды в одном окне (100→20→50→5→35): каждый переход считается заново
// от новой базы, итог 30+30=60, не 100+30-30+30 и не отрицательное число.
func TestAggregateIncreaseDoubleCounterReset(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 7113
	seedSumCumulative(t, conn, pid, "double_reset.total", "prod", now.Add(-4*time.Minute), 100)
	seedSumCumulative(t, conn, pid, "double_reset.total", "prod", now.Add(-3*time.Minute), 20)
	seedSumCumulative(t, conn, pid, "double_reset.total", "prod", now.Add(-2*time.Minute), 50)
	seedSumCumulative(t, conn, pid, "double_reset.total", "prod", now.Add(-time.Minute), 5)
	seedSumCumulative(t, conn, pid, "double_reset.total", "prod", now, 35)

	got, ok, err := q.Aggregate(ctx, pid, "double_reset.total", "prod", "", nil, "increase",
		now.Add(-10*time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Aggregate increase double reset: %v", err)
	}
	if !ok {
		t.Fatal("Aggregate increase double reset: ok=false, want true (has data)")
	}
	if got < 59.9 || got > 60.1 {
		t.Fatalf("increase при двойном сбросе = %v, want 60 (30+30)", got)
	}
}

// delta-temporality sum: значения уже точечные приращения, increase обязан
// совпасть с sum, а не тихо съехать на avg через общий default в scalarAggExpr.
func TestAggregateIncreaseNonCumulativeSumMatchesSum(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 7114
	seedSumDelta(t, conn, pid, "delta.count", "prod", now.Add(-2*time.Minute), 5)
	seedSumDelta(t, conn, pid, "delta.count", "prod", now.Add(-time.Minute), 7)
	seedSumDelta(t, conn, pid, "delta.count", "prod", now, 3)

	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)
	wantSum, ok, err := q.Aggregate(ctx, pid, "delta.count", "prod", "", nil, "sum", from, to)
	if err != nil {
		t.Fatalf("Aggregate sum: %v", err)
	}
	if !ok || wantSum < 14.9 || wantSum > 15.1 {
		t.Fatalf("Aggregate sum = %v (ok=%v), want 15", wantSum, ok)
	}

	gotIncrease, ok, err := q.Aggregate(ctx, pid, "delta.count", "prod", "", nil, "increase", from, to)
	if err != nil {
		t.Fatalf("Aggregate increase: %v", err)
	}
	if !ok {
		t.Fatal("Aggregate increase: ok=false, want true (has data)")
	}
	if gotIncrease != wantSum {
		t.Fatalf("increase = %v, want совпадение с sum = %v (delta-точки уже приращения)", gotIncrease, wantSum)
	}
}
