package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestResultWriterInsertsIntoClickHouseAndCloseFlushesRemainder(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	w := uptime.NewResultWriter(conn)
	go w.Run()

	at := time.Now().UTC().Truncate(time.Millisecond)
	w.Add(101, 202, "local", at, uptime.Result{
		OK: true, StatusCode: 200, TotalMs: 123, TTFBMs: 45, ConnectMs: 10, BodySize: 512,
	})
	w.Add(101, 202, "eu", at, uptime.Result{
		OK: false, StatusCode: 0, Error: "timeout after 5s", TotalMs: 5000,
	})

	// Close (without any prior tick/kick) must flush whatever is still
	// sitting in the buffer.
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var cnt uint64
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM check_results WHERE project_id = 101 AND monitor_id = 202").Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("count = %d, want 2", cnt)
	}

	var ok uint8
	var totalMs uint32
	var region string
	if err := conn.QueryRow(ctx,
		"SELECT ok, total_ms, region FROM check_results WHERE project_id = 101 AND monitor_id = 202 AND region = 'local'",
	).Scan(&ok, &totalMs, &region); err != nil {
		t.Fatalf("select local row: %v", err)
	}
	if ok != 1 || totalMs != 123 || region != "local" {
		t.Fatalf("local row: ok=%d total_ms=%d region=%q, want ok=1 total_ms=123 region=local", ok, totalMs, region)
	}

	var ok2 uint8
	var errText string
	if err := conn.QueryRow(ctx,
		"SELECT ok, error FROM check_results WHERE project_id = 101 AND monitor_id = 202 AND region = 'eu'",
	).Scan(&ok2, &errText); err != nil {
		t.Fatalf("select eu row: %v", err)
	}
	if ok2 != 0 || errText != "timeout after 5s" {
		t.Fatalf("eu row: ok=%d error=%q, want ok=0 error=%q", ok2, errText, "timeout after 5s")
	}

	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}
}
