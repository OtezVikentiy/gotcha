package metric

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// seedSumCumulative вставляет точку monotonic cumulative sum-счётчика (для
// rateSeries-ветки Series).
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

// seedHistogram вставляет histogram-точку (для Aggregate percentile-ветки).
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

// TestQueryRateSeries: monotonic cumulative sum-счётчик должен пойти по ветке
// rateSeries — max(value) по бакету, затем дельта соседних бакетов / шаг.
// Кумулятив 100→160 за минуту → rate = 1/s.
func TestQueryRateSeries(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	const pid = 71
	// Два бакета по минуте: кумулятив 100 и 160.
	seedSumCumulative(t, conn, pid, "req.total", "prod", now.Add(-2*time.Minute), 100)
	seedSumCumulative(t, conn, pid, "req.total", "prod", now.Add(-1*time.Minute), 160)

	pts, err := q.Series(ctx, pid, "req.total", "prod", LabelMatcher{}, "avg",
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

// TestQueryRateSeriesSingleBucket: единственный бакет (len(cum)<2) → rateSeries
// возвращает nil (дельту не из чего считать).
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

	pts, err := q.Series(ctx, pid, "req.one", "prod", LabelMatcher{}, "avg",
		now.Add(-10*time.Minute), now.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("rate Series single: %v", err)
	}
	if len(pts) != 0 {
		t.Fatalf("single-bucket rate points = %+v, want 0", pts)
	}
}

// TestAggregateHistogramPercentile: Aggregate по histogram+p95 должен просуммировать
// bucket_counts всего окна и вернуть квантиль, ok=true.
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

	v, ok, err := q.Aggregate(ctx, pid, "http.dur", "prod", LabelMatcher{}, "p95",
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

// TestAggregateNoData: metricType находит метрику (по project+name без ts), но в
// окне [from,to) точек нет → count()=0 → ok=false (ветка «нет данных»).
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

	v, ok, err := q.Aggregate(ctx, pid, "cpu", "prod", LabelMatcher{}, "avg",
		now.Add(-5*time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Aggregate no-data: %v", err)
	}
	if ok {
		t.Fatalf("Aggregate no-data: ok=true (v=%v), want false", v)
	}
}
