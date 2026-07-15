package metric_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestWriterFlushesToCH(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	conn := testenv.MigratedCH(t)
	w := metric.NewWriter(conn)
	go w.Run()

	now := time.Now().UTC()
	w.Add(7, metric.MetricPoint{Name: "cpu", Type: "gauge", Unit: "1", Service: "api", Environment: "prod",
		Attributes: map[string]string{"host": "h1"}, TS: now, Value: 0.5})
	w.Add(7, metric.MetricPoint{Name: "reqs", Type: "sum", TS: now, Value: 10, Monotonic: true, Temporality: "cumulative"})
	w.Add(7, metric.MetricPoint{Name: "dur", Type: "histogram", TS: now, Value: 240, Count: 12,
		BucketCounts: []uint64{2, 8, 2}, ExplicitBounds: []float64{100, 500}})

	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	ctx := context.Background()
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM metric_points WHERE project_id=7").Scan(&n); err != nil || n != 3 {
		t.Fatalf("count = %d err=%v, want 3", n, err)
	}
	// Array-поля гистограммы доехали.
	var bounds []float64
	if err := conn.QueryRow(ctx,
		"SELECT explicit_bounds FROM metric_points WHERE project_id=7 AND name='dur'").Scan(&bounds); err != nil {
		t.Fatalf("select bounds: %v", err)
	}
	if len(bounds) != 2 || bounds[0] != 100 || bounds[1] != 500 {
		t.Fatalf("bounds = %v", bounds)
	}
	// Map-поле лейблов доехало.
	var host string
	if err := conn.QueryRow(ctx,
		"SELECT attributes['host'] FROM metric_points WHERE project_id=7 AND name='cpu'").Scan(&host); err != nil || host != "h1" {
		t.Fatalf("host label = %q err=%v", host, err)
	}
}
