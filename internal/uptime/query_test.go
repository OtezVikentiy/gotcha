package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestQueryReadsFromClickHouse(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const projectID = int64(31)
	const monitorID = int64(301)
	const otherMonitorID = int64(302)

	w := uptime.NewResultWriter(conn)
	go w.Run()

	now := time.Now().UTC()
	// Window well in the past so it never collides with "now" queries.
	windowFrom := now.Truncate(10 * time.Minute).Add(-3 * time.Hour)
	windowTo := windowFrom.Add(time.Hour)

	// 10 checks 5 minutes apart, spanning the first 50 minutes of the
	// window (last 10 minutes stay empty on purpose, to exercise
	// zero-filling). First two fail, remaining eight succeed.
	for i := 0; i < 10; i++ {
		at := windowFrom.Add(time.Duration(i) * 5 * time.Minute)
		ok := i >= 2
		res := uptime.Result{
			OK:         ok,
			StatusCode: 200,
			TotalMs:    uint32(100 + i*10),
			DNSMs:      uint32(5 + i),
			ConnectMs:  uint32(10 + i),
			TLSMs:      uint32(15 + i),
			TTFBMs:     uint32(50 + i),
		}
		if !ok {
			res.StatusCode = 500
			res.Error = "boom"
		}
		w.Add(projectID, monitorID, "local", at, res)
	}

	// A second monitor, used for UptimeBatch: 1 ok + 1 fail.
	w.Add(projectID, otherMonitorID, "local", windowFrom.Add(time.Minute), uptime.Result{
		OK: true, StatusCode: 200, TotalMs: 42,
	})
	w.Add(projectID, otherMonitorID, "local", windowFrom.Add(2*time.Minute), uptime.Result{
		OK: false, StatusCode: 500, Error: "x", TotalMs: 99,
	})

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	q := uptime.NewQuery(conn)

	t.Run("Uptime", func(t *testing.T) {
		stat, err := q.Uptime(ctx, monitorID, windowFrom, windowTo, nil)
		if err != nil {
			t.Fatalf("Uptime: %v", err)
		}
		if stat.Total != 10 || stat.OK != 8 {
			t.Fatalf("Uptime = %+v, want Total=10 OK=8", stat)
		}
		if got := stat.Ratio(); got != 0.8 {
			t.Fatalf("Ratio() = %v, want 0.8", got)
		}

		// Exclude a maintenance window covering exactly the two failed
		// checks (windowFrom and windowFrom+5m): remaining 8/8.
		excl := []uptime.Interval{{From: windowFrom, To: windowFrom.Add(10 * time.Minute)}}
		stat2, err := q.Uptime(ctx, monitorID, windowFrom, windowTo, excl)
		if err != nil {
			t.Fatalf("Uptime with exclude: %v", err)
		}
		if stat2.Total != 8 || stat2.OK != 8 {
			t.Fatalf("Uptime with exclude = %+v, want Total=8 OK=8", stat2)
		}
		if got := stat2.Ratio(); got != 1.0 {
			t.Fatalf("Ratio() with exclude = %v, want 1.0", got)
		}
	})

	t.Run("UptimeStatZeroRatio", func(t *testing.T) {
		var empty uptime.UptimeStat
		if got := empty.Ratio(); got != 0 {
			t.Fatalf("Ratio() on zero-total stat = %v, want 0", got)
		}
	})

	t.Run("Latency", func(t *testing.T) {
		points, err := q.Latency(ctx, monitorID, windowFrom, windowTo, 10*time.Minute)
		if err != nil {
			t.Fatalf("Latency: %v", err)
		}
		if len(points) != 7 {
			t.Fatalf("len(points) = %d, want 7", len(points))
		}
		for i := 1; i < len(points); i++ {
			if !points[i].T.After(points[i-1].T) {
				t.Fatalf("points not in chronological order: %v", points)
			}
		}
		if !points[0].T.Equal(windowFrom) {
			t.Fatalf("points[0].T = %v, want %v", points[0].T, windowFrom)
		}

		// First bucket [windowFrom, +10m) holds checks i=0 (100ms) and
		// i=1 (110ms): avg total_ms = 105.
		if points[0].AvgTotalMs != 105 {
			t.Fatalf("points[0].AvgTotalMs = %d, want 105", points[0].AvgTotalMs)
		}
		if points[0].AvgDNSMs == 0 || points[0].AvgConnectMs == 0 || points[0].AvgTLSMs == 0 || points[0].AvgTTFBMs == 0 {
			t.Fatalf("points[0] has zero averages: %+v", points[0])
		}

		// Last 10 minutes of the window have no checks: zero-filled.
		last := points[len(points)-1]
		if last.AvgTotalMs != 0 || last.AvgDNSMs != 0 {
			t.Fatalf("last bucket not zero-filled: %+v", last)
		}
	})

	t.Run("Recent", func(t *testing.T) {
		rows, err := q.Recent(ctx, monitorID, 5)
		if err != nil {
			t.Fatalf("Recent: %v", err)
		}
		if len(rows) != 5 {
			t.Fatalf("len(rows) = %d, want 5", len(rows))
		}
		for i := 1; i < len(rows); i++ {
			if rows[i].Timestamp.After(rows[i-1].Timestamp) {
				t.Fatalf("rows not in DESC order: %v", rows)
			}
		}
		// Most recent check is i=9: ok=true, status=200, total_ms=190.
		first := rows[0]
		if !first.OK || first.StatusCode != 200 || first.TotalMs != 190 {
			t.Fatalf("unexpected most recent row: %+v", first)
		}
		if first.Region != "local" {
			t.Fatalf("unexpected region: %q", first.Region)
		}

		all, err := q.Recent(ctx, monitorID, 100)
		if err != nil {
			t.Fatalf("Recent (all): %v", err)
		}
		if len(all) != 10 {
			t.Fatalf("len(all) = %d, want 10", len(all))
		}
		// First two (oldest) failed.
		last := all[len(all)-1]
		if last.OK || last.StatusCode != 500 || last.Error != "boom" {
			t.Fatalf("unexpected oldest row: %+v", last)
		}
	})

	t.Run("Bars", func(t *testing.T) {
		bars, err := q.Bars(ctx, monitorID, windowFrom, windowTo, 24)
		if err != nil {
			t.Fatalf("Bars: %v", err)
		}
		if len(bars) != 24 {
			t.Fatalf("len(bars) = %d, want 24", len(bars))
		}
		var total, ok uint64
		for _, b := range bars {
			total += b.Total
			ok += b.OK
		}
		if total != 10 || ok != 8 {
			t.Fatalf("sum(bars) = total=%d ok=%d, want total=10 ok=8", total, ok)
		}
		// The window's last 10 minutes have no checks, so at least one
		// trailing bucket must be structurally zero.
		var zeros int
		for _, b := range bars {
			if b.Total == 0 {
				zeros++
			}
		}
		if zeros == 0 {
			t.Fatalf("expected some zero-filled buckets, got none: %v", bars)
		}
	})

	t.Run("UptimeBatch", func(t *testing.T) {
		got, err := q.UptimeBatch(ctx, []int64{monitorID, otherMonitorID}, windowFrom, windowTo)
		if err != nil {
			t.Fatalf("UptimeBatch: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len(got) = %d, want 2", len(got))
		}
		if got[monitorID].Total != 10 || got[monitorID].OK != 8 {
			t.Fatalf("got[monitorID] = %+v, want Total=10 OK=8", got[monitorID])
		}
		if got[otherMonitorID].Total != 2 || got[otherMonitorID].OK != 1 {
			t.Fatalf("got[otherMonitorID] = %+v, want Total=2 OK=1", got[otherMonitorID])
		}
	})

	t.Run("UptimeBatchEmpty", func(t *testing.T) {
		got, err := q.UptimeBatch(ctx, nil, windowFrom, windowTo)
		if err != nil {
			t.Fatalf("UptimeBatch(nil): %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("UptimeBatch(nil) = %v, want empty", got)
		}
	})
}
