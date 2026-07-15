package metric

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestHistogramQuantile(t *testing.T) {
	counts := []uint64{2, 8, 2}
	bounds := []float64{100, 500}
	p50 := histogramQuantile(counts, bounds, 0.5)
	if p50 < 100 || p50 > 500 {
		t.Fatalf("p50=%v, want in (100,500]", p50)
	}
	p95 := histogramQuantile(counts, bounds, 0.95)
	if p95 < p50 {
		t.Fatalf("p95 %v < p50 %v", p95, p50)
	}
	p99 := histogramQuantile(counts, bounds, 0.99)
	if p99 < p95 {
		t.Fatalf("p99 %v < p95 %v", p99, p95)
	}
	// Пустая гистограмма → 0.
	if v := histogramQuantile(nil, nil, 0.5); v != 0 {
		t.Fatalf("empty = %v, want 0", v)
	}
}

// seedPoints вставляет точки напрямую (без writer) для query-тестов.
func seedGauge(t *testing.T, conn interface {
	Exec(ctx context.Context, query string, args ...any) error
}, projectID int64, name, env string, ts time.Time, val float64, attrs map[string]string) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	if err := conn.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'gauge', '1', 'api', ?, ?, ?, ?, 0, [], [], 0, '')`,
		projectID, name, env, attrs, ts, val); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}
}

func TestQueryHistogramSeries(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	// Две histogram-точки в одном бакете: суммарно counts [4,16,4], bounds [100,500].
	for i := 0; i < 2; i++ {
		if err := conn.Exec(ctx, `
			INSERT INTO metric_points (project_id, name, type, unit, service, environment, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
			VALUES (11, 'http.dur', 'histogram', 'ms', 'api', 'prod', map(), ?, 240, 12, [2,8,2], [100,500], 0, 'cumulative')`,
			now.Add(-time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("seed hist: %v", err)
		}
	}
	pts, err := q.Series(ctx, 11, "http.dur", "prod", LabelMatcher{}, "p95", now.Add(-5*time.Minute), now.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("histogram Series: %v", err)
	}
	if len(pts) == 0 {
		t.Fatalf("no histogram points")
	}
	// p95 должен попасть в последний бакет (>= 500 суррогат) или его окрестность.
	if pts[len(pts)-1].V < 100 {
		t.Fatalf("p95 = %v, want >= 100", pts[len(pts)-1].V)
	}
}

func TestQueryListAndSeries(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	q := NewQuery(conn)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Minute)
	// Две метрики, разные env и лейблы.
	seedGauge(t, conn, 9, "cpu", "prod", now.Add(-3*time.Minute), 0.2, map[string]string{"host": "h1"})
	seedGauge(t, conn, 9, "cpu", "prod", now.Add(-1*time.Minute), 0.6, map[string]string{"host": "h1"})
	seedGauge(t, conn, 9, "cpu", "stage", now.Add(-1*time.Minute), 0.9, map[string]string{"host": "h2"})
	seedGauge(t, conn, 9, "mem", "prod", now.Add(-1*time.Minute), 100, nil)

	// ListMetrics.
	metrics, err := q.ListMetrics(ctx, 9, "")
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}
	if len(metrics) != 2 {
		t.Fatalf("metrics = %+v, want 2", metrics)
	}
	// Фильтр по env=stage → только cpu.
	stageMetrics, _ := q.ListMetrics(ctx, 9, "stage")
	if len(stageMetrics) != 1 || stageMetrics[0].Name != "cpu" {
		t.Fatalf("stage metrics = %+v", stageMetrics)
	}

	// Labels cpu → host: h1,h2.
	labels, err := q.Labels(ctx, 9, "cpu")
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(labels["host"]) != 2 {
		t.Fatalf("host labels = %v", labels["host"])
	}

	// Environments cpu → prod,stage.
	envs, _ := q.Environments(ctx, 9, "cpu")
	if len(envs) != 2 {
		t.Fatalf("envs = %v", envs)
	}

	// Series cpu avg по prod за окно, шаг 1m → есть точки.
	pts, err := q.Series(ctx, 9, "cpu", "prod", LabelMatcher{}, "avg", now.Add(-10*time.Minute), now.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(pts) == 0 {
		t.Fatalf("Series returned no points")
	}
	// Матчер по host=h2 (только stage-точка) + env stage.
	pts2, err := q.Series(ctx, 9, "cpu", "stage", LabelMatcher{Key: "host", Value: "h2"}, "max", now.Add(-10*time.Minute), now.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("Series matcher: %v", err)
	}
	if len(pts2) != 1 || pts2[0].V != 0.9 {
		t.Fatalf("matched series = %+v", pts2)
	}
}
