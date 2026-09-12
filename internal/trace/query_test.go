package trace_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

func TestQueryReadsFromClickHouse(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const projectID = int64(55)
	const projectID2 = int64(56)
	const projectID3 = int64(57)
	const projectID4 = int64(58)
	const projectID5 = int64(59)
	const projectID6 = int64(60)
	const projectID7 = int64(61)

	w := trace.NewSpanWriter(conn)
	go w.Run()

	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	from := base
	to := base.Add(time.Hour)

	for i := 0; i < 100; i++ {
		status := "ok"
		if i < 10 {
			status = "internal_error"
		}
		at := base.Add(time.Duration(i) * 36 * time.Second)
		dur := time.Duration(i+1) * 1000 * time.Microsecond
		w.Add(projectID, projectID, trace.Transaction{
			TraceID:     fmt.Sprintf("users-%03d", i),
			SpanID:      fmt.Sprintf("uspan-%03d", i),
			Name:        "GET /api/users",
			Op:          "http.server",
			Status:      status,
			Start:       at,
			End:         at.Add(dur),
			Environment: "production",
		})
	}

	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		w.Add(projectID, projectID, trace.Transaction{
			TraceID:     fmt.Sprintf("users-stg-%d", i),
			SpanID:      fmt.Sprintf("uspan-stg-%d", i),
			Name:        "GET /api/users",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(1 * time.Millisecond),
			Environment: "staging",
		})
	}

	for i := 0; i < 20; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		w.Add(projectID, projectID, trace.Transaction{
			TraceID:     fmt.Sprintf("orders-%02d", i),
			SpanID:      fmt.Sprintf("ospan-%02d", i),
			Name:        "GET /api/orders",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(2 * time.Millisecond),
			Environment: "production",
		})
	}

	const wfTrace = "waterfall-trace-id"
	wfStart := base.Add(10 * time.Minute)
	w.Add(projectID, projectID, trace.Transaction{
		TraceID:     wfTrace,
		SpanID:      "wf-root",
		Name:        "GET /api/checkout",
		Op:          "http.server",
		Status:      "ok",
		Start:       wfStart,
		End:         wfStart.Add(300 * time.Millisecond),
		Environment: "production",
		Spans: []trace.Span{
			{SpanID: "wf-db", ParentSpanID: "wf-root", Op: "db.query", Description: "SELECT 1",
				Start: wfStart.Add(20 * time.Millisecond), End: wfStart.Add(80 * time.Millisecond), Status: "ok"},
			{SpanID: "wf-http", ParentSpanID: "wf-root", Op: "http.client", Description: "GET https://x/y",
				Start: wfStart.Add(90 * time.Millisecond), End: wfStart.Add(150 * time.Millisecond), Status: "ok"},
		},
	})

	w.Add(projectID5, projectID5, trace.Transaction{
		TraceID: "off-trace", SpanID: "off-root", Name: "POST /pay", Op: "http.server",
		Status: "ok", Start: wfStart, End: wfStart.Add(950 * time.Millisecond), Environment: "production",
		Spans: []trace.Span{
			{SpanID: "offdb1", ParentSpanID: "off-root", Op: "db.sql.query",
				Description: "SELECT * FROM payments JOIN ledger ON ledger.id = payments.id",
				Start:       wfStart, End: wfStart.Add(900 * time.Millisecond), Status: "ok",
				Data: map[string]any{"db.system": "postgresql", "code.filepath": "app/pay.py", "code.lineno": 42}},
		},
	})

	for i := 0; i < 100; i++ {
		at := base.Add(time.Duration(i) * 3 * time.Second)
		dur := time.Duration(i+1) * 1000 * time.Microsecond
		w.Add(projectID2, projectID2, trace.Transaction{
			TraceID:     fmt.Sprintf("lat-%03d", i),
			SpanID:      fmt.Sprintf("latspan-%03d", i),
			Name:        "GET /lat",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(dur),
			Environment: "production",
		})
	}

	for i := 0; i < 2; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID2, projectID2, trace.Transaction{
			TraceID:     fmt.Sprintf("apdex-on-%d", i),
			SpanID:      fmt.Sprintf("apdexspan-on-%d", i),
			Name:        "GET /apdex",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(200 * time.Millisecond),
			Environment: "production",
		})
	}
	for i := 0; i < 2; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID2, projectID2, trace.Transaction{
			TraceID:     fmt.Sprintf("apdex-over-%d", i),
			SpanID:      fmt.Sprintf("apdexspan-over-%d", i),
			Name:        "GET /apdex",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(200*time.Millisecond + time.Microsecond),
			Environment: "production",
		})
	}

	const bigTrace = "big-trace-id"
	bigStart := base.Add(15 * time.Minute)
	bigSpans := make([]trace.Span, 5100)
	for i := range bigSpans {
		st := bigStart.Add(time.Duration(i) * time.Millisecond)
		bigSpans[i] = trace.Span{
			SpanID:       fmt.Sprintf("big-%04d", i),
			ParentSpanID: "big-root",
			Op:           "db.query",
			Description:  "SELECT 1",
			Start:        st,
			End:          st.Add(time.Millisecond),
			Status:       "ok",
		}
	}
	w.Add(projectID2, projectID2, trace.Transaction{
		TraceID:     bigTrace,
		SpanID:      "big-root",
		Name:        "GET /big",
		Op:          "http.server",
		Status:      "ok",
		Start:       bigStart,
		End:         bigStart.Add(6 * time.Second),
		Environment: "production",
		Spans:       bigSpans,
	})

	for i, lcp := range []float64{2000, 2400, 2600} {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID3, projectID3, trace.Transaction{
			TraceID:      fmt.Sprintf("home-%d", i),
			SpanID:       fmt.Sprintf("homespan-%d", i),
			Name:         "GET /home",
			Op:           "pageload",
			Status:       "ok",
			Start:        at,
			End:          at.Add(time.Second),
			Environment:  "production",
			Measurements: map[string]float64{"lcp": lcp, "cls": 0.05},
		})
	}
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID3, projectID3, trace.Transaction{
			TraceID:      fmt.Sprintf("slow-%d", i),
			SpanID:       fmt.Sprintf("slowspan-%d", i),
			Name:         "GET /slow",
			Op:           "pageload",
			Status:       "ok",
			Start:        at,
			End:          at.Add(time.Second),
			Environment:  "production",
			Measurements: map[string]float64{"lcp": 5000},
		})
	}
	for i := 0; i < 2; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID3, projectID3, trace.Transaction{
			TraceID:      fmt.Sprintf("home-stg-%d", i),
			SpanID:       fmt.Sprintf("homestgspan-%d", i),
			Name:         "GET /home",
			Op:           "pageload",
			Status:       "ok",
			Start:        at,
			End:          at.Add(time.Second),
			Environment:  "staging",
			Measurements: map[string]float64{"lcp": 9000},
		})
	}

	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID3, projectID3, trace.Transaction{
			TraceID:     fmt.Sprintf("noop-%d", i),
			SpanID:      fmt.Sprintf("noopspan-%d", i),
			Name:        "GET /api/noop",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(20 * time.Millisecond),
			Environment: "production",
		})
	}

	regNow := time.Now().UTC().Truncate(24 * time.Hour).Add(-12 * time.Hour)
	regRecentFrom := regNow.Add(-time.Hour)
	regRecentTo := regNow
	regRecentAt := regNow.Add(-30 * time.Minute)

	regDays := []struct {
		at    time.Time
		durMs int
		n     int
	}{
		{regRecentAt, 1000, 50},
		{regNow.Add(-24 * time.Hour), 200, 40},
		{regNow.Add(-48 * time.Hour), 300, 40},
	}
	for di, d := range regDays {
		for i := 0; i < d.n; i++ {
			w.Add(projectID4, projectID4, trace.Transaction{
				TraceID:     fmt.Sprintf("reg-%d-%03d", di, i),
				SpanID:      fmt.Sprintf("regspan-%d-%03d", di, i),
				Name:        "GET /reg",
				Op:          "http.server",
				Status:      "ok",
				Start:       d.at,
				End:         d.at.Add(time.Duration(d.durMs) * time.Millisecond),
				Environment: "production",
			})
		}
	}

	vDays := []struct {
		at  time.Time
		lcp float64
		n   int
	}{
		{regRecentAt, 2000, 30},
		{regNow.Add(-24 * time.Hour), 500, 20},
		{regNow.Add(-48 * time.Hour), 800, 20},
	}
	for di, d := range vDays {
		for i := 0; i < d.n; i++ {
			w.Add(projectID4, projectID4, trace.Transaction{
				TraceID:      fmt.Sprintf("vp-%d-%03d", di, i),
				SpanID:       fmt.Sprintf("vpspan-%d-%03d", di, i),
				Name:         "GET /vpage",
				Op:           "pageload",
				Status:       "ok",
				Start:        d.at,
				End:          d.at.Add(time.Second),
				Environment:  "production",
				Measurements: map[string]float64{"lcp": d.lcp},
			})
		}
	}

	for i := 0; i < 5; i++ {
		w.Add(projectID4, projectID4, trace.Transaction{
			TraceID:      fmt.Sprintf("vp2-%03d", i),
			SpanID:       fmt.Sprintf("vp2span-%03d", i),
			Name:         "GET /vpage2",
			Op:           "pageload",
			Status:       "ok",
			Start:        regRecentAt,
			End:          regRecentAt.Add(time.Second),
			Environment:  "production",
			Measurements: map[string]float64{"lcp": 100},
		})
	}

	depsAt := base.Add(30 * time.Minute)
	w.Add(projectID6, projectID6, trace.Transaction{
		TraceID: "deps-trace", SpanID: "deps-root", Name: "GET /api/checkout", Op: "http.server",
		Status: "ok", Start: depsAt, End: depsAt.Add(200 * time.Millisecond), Environment: "production",
		Spans: []trace.Span{
			{SpanID: "deps-db1", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "SELECT id FROM orders WHERE id = $1",
				Start:       depsAt, End: depsAt.Add(3000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "postgresql"}},
			{SpanID: "deps-db2", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "internal_error",
				Description: "INSERT INTO orders (id) VALUES ($1)",
				Start:       depsAt, End: depsAt.Add(9000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "postgresql"}},
			{SpanID: "deps-ro1", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "SELECT 1",
				Start:       depsAt, End: depsAt.Add(1000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "readonly-db"}},
			{SpanID: "deps-ro2", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "WITH x AS (SELECT 1) SELECT * FROM x",
				Start:       depsAt, End: depsAt.Add(1000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "readonly-db"}},
			{SpanID: "deps-ro3", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "select * from t",
				Start:       depsAt, End: depsAt.Add(1000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "readonly-db"}},
			{SpanID: "deps-ro4", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "   SELECT 1",
				Start:       depsAt, End: depsAt.Add(1000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "readonly-db"}},
			{SpanID: "deps-txn", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "BEGIN",
				Start:       depsAt, End: depsAt.Add(100 * time.Microsecond),
				Data: map[string]any{"db.system.name": "sqlite"}},
			{SpanID: "deps-txn2", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "1 SELECT",
				Start:       depsAt, End: depsAt.Add(100 * time.Microsecond),
				Data: map[string]any{"db.system.name": "sqlite"}},
			{SpanID: "deps-oldop", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "SELECT 1",
				Start:       depsAt, End: depsAt.Add(1000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "oldop-db", "db.operation": "UPDATE"}},
			{SpanID: "deps-emptyop", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "SELECT 1",
				Start:       depsAt, End: depsAt.Add(1000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "emptyop-db", "db.operation": ""}},
			{SpanID: "deps-crossattr-db", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "SELECT 1",
				Start:       depsAt, End: depsAt.Add(1000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "crossattr-db", "http.request.method": "POST"}},
			{SpanID: "deps-redis", ParentSpanID: "deps-root", Op: "db.redis", Status: "ok",
				Description: "HGET k",
				Start:       depsAt, End: depsAt.Add(500 * time.Microsecond)},
			{SpanID: "deps-redis2", ParentSpanID: "deps-root", Op: "db.redis", Status: "ok",
				Description: "SET k v",
				Start:       depsAt, End: depsAt.Add(500 * time.Microsecond)},
			{SpanID: "deps-memcached", ParentSpanID: "deps-root", Op: "db.memcached", Status: "ok",
				Description: "get k",
				Start:       depsAt, End: depsAt.Add(300 * time.Microsecond)},
			{SpanID: "deps-memcached2", ParentSpanID: "deps-root", Op: "db.memcached", Status: "ok",
				Description: "flush_all",
				Start:       depsAt, End: depsAt.Add(300 * time.Microsecond)},
			{SpanID: "deps-mysql", ParentSpanID: "deps-root", Op: "db.sql.query", Status: "ok",
				Description: "SELECT 1",
				Start:       depsAt, End: depsAt.Add(2000 * time.Microsecond),
				Data: map[string]any{"db.system": "mysql", "db.operation.name": "INSERT"}},
			{SpanID: "deps-stripe", ParentSpanID: "deps-root", Op: "http.client", Status: "ok",
				Description: "POST https://api.stripe.com/v1/charges",
				Start:       depsAt, End: depsAt.Add(60000 * time.Microsecond),
				Data: map[string]any{"server.address": "api.stripe.com"}},
			{SpanID: "deps-cdn", ParentSpanID: "deps-root", Op: "http.client", Status: "ok",
				Description: "GET https://cdn.example.com/asset.js",
				Start:       depsAt, End: depsAt.Add(30000 * time.Microsecond),
				Data: map[string]any{"url.full": "https://cdn.example.com/asset.js"}},
			{SpanID: "deps-legacy", ParentSpanID: "deps-root", Op: "http.client", Status: "ok",
				Start: depsAt, End: depsAt.Add(10000 * time.Microsecond),
				Data: map[string]any{"http.method": "POST", "server.address": "legacy.example.com"}},
			{SpanID: "deps-crossattr-http", ParentSpanID: "deps-root", Op: "http.client", Status: "ok",
				Description: "GET https://x.example.com/",
				Start:       depsAt, End: depsAt.Add(10000 * time.Microsecond),
				Data: map[string]any{"db.operation.name": "INSERT", "server.address": "x.example.com"}},
			{SpanID: "deps-dbhost-a", ParentSpanID: "deps-root", Op: "http.client", Status: "ok",
				Start: depsAt, End: depsAt.Add(5000 * time.Microsecond),
				Data: map[string]any{"server.address": "db.internal:5432"}},
			{SpanID: "deps-dbhost-b", ParentSpanID: "deps-root", Op: "http.client", Status: "ok",
				Start: depsAt, End: depsAt.Add(5000 * time.Microsecond),
				Data: map[string]any{"url.full": "https://db.internal:5432/probe"}},
			{SpanID: "deps-httpserver", ParentSpanID: "deps-root", Op: "http.server", Status: "ok",
				Start: depsAt, End: depsAt.Add(120000 * time.Microsecond)},
			{SpanID: "deps-render", ParentSpanID: "deps-root", Op: "view.render", Status: "ok",
				Start: depsAt, End: depsAt.Add(1000 * time.Microsecond)},
		},
	})
	w.Add(projectID7, projectID7, trace.Transaction{
		TraceID: "deps-other-trace", SpanID: "deps-other-root", Name: "GET /x", Op: "http.server",
		Status: "ok", Start: depsAt, End: depsAt.Add(100 * time.Millisecond), Environment: "production",
		Spans: []trace.Span{
			{SpanID: "deps-other-db", ParentSpanID: "deps-other-root", Op: "db.sql.query", Status: "ok",
				Start: depsAt, End: depsAt.Add(1000 * time.Microsecond),
				Data: map[string]any{"db.system.name": "oracle"}},
		},
	})

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	q := trace.NewQuery(conn)

	t.Run("Endpoints", func(t *testing.T) {
		got, _, err := q.Endpoints(ctx, projectID, from, to, "production", 50)
		if err != nil {
			t.Fatalf("Endpoints: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3 (%+v)", len(got), got)
		}
		if got[0].Transaction != "GET /api/users" || got[1].Transaction != "GET /api/orders" ||
			got[2].Transaction != "GET /api/checkout" {
			t.Fatalf("order: %q, %q, %q", got[0].Transaction, got[1].Transaction, got[2].Transaction)
		}
		users := got[0]
		if users.Count != 100 {
			t.Fatalf("users.Count = %d, want 100", users.Count)
		}
		if users.Throughput < 1.66 || users.Throughput > 1.67 {
			t.Fatalf("users.Throughput = %v, want ~1.667", users.Throughput)
		}
		if users.FailureRate < 0.099 || users.FailureRate > 0.101 {
			t.Fatalf("users.FailureRate = %v, want 0.10", users.FailureRate)
		}
		assertNear(t, "p50", users.P50, 50500, 2)
		assertNear(t, "p75", users.P75, 75250, 2)
		assertNear(t, "p95", users.P95, 95050, 2)
		assertNear(t, "p99", users.P99, 99010, 2)
		if users.ApdexScore < 0.749 || users.ApdexScore > 0.751 {
			t.Fatalf("users.ApdexScore = %v, want 0.75", users.ApdexScore)
		}
	})

	t.Run("EndpointsEnvironmentFilter", func(t *testing.T) {
		stg, _, err := q.Endpoints(ctx, projectID, from, to, "staging", 50)
		if err != nil {
			t.Fatalf("Endpoints staging: %v", err)
		}
		if len(stg) != 1 || stg[0].Transaction != "GET /api/users" || stg[0].Count != 5 {
			t.Fatalf("staging endpoints = %+v, want single users with count 5", stg)
		}

		all, _, err := q.Endpoints(ctx, projectID, from, to, "", 50)
		if err != nil {
			t.Fatalf("Endpoints all: %v", err)
		}
		var usersAll uint64
		for _, e := range all {
			if e.Transaction == "GET /api/users" {
				usersAll = e.Count
			}
		}
		if usersAll != 105 {
			t.Fatalf("users count without env filter = %d, want 105", usersAll)
		}
	})

	t.Run("EndpointsEmptyProject", func(t *testing.T) {
		got, _, err := q.Endpoints(ctx, 999999, from, to, "", 50)
		if err != nil {
			t.Fatalf("Endpoints empty: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("len(got) = %d, want 0 for unknown project", len(got))
		}
	})

	t.Run("Dependencies", func(t *testing.T) {
		deps, err := q.Dependencies(ctx, projectID6, from, to, 50)
		if err != nil {
			t.Fatalf("Dependencies: %v", err)
		}
		byTarget := map[string]trace.Dependency{}
		for _, d := range deps {
			byTarget[d.Target] = d
		}
		pg := byTarget["postgresql"]
		if pg.Kind != "database" {
			t.Fatalf("postgresql.Kind = %q, want database", pg.Kind)
		}
		if pg.Calls != 2 {
			t.Fatalf("postgresql.Calls = %d, want 2", pg.Calls)
		}
		if pg.ErrorRate < 0.499 || pg.ErrorRate > 0.501 {
			t.Fatalf("postgresql.ErrorRate = %v, want 0.5", pg.ErrorRate)
		}
		if byTarget["mysql"].Kind != "database" {
			t.Fatalf("mysql.Kind = %q, want database", byTarget["mysql"].Kind)
		}
		if byTarget["redis"].Kind != "cache" {
			t.Fatalf("redis.Kind = %q, want cache", byTarget["redis"].Kind)
		}
		if byTarget["api.stripe.com"].Kind != "http" {
			t.Fatalf("api.stripe.com.Kind = %q, want http", byTarget["api.stripe.com"].Kind)
		}
		if byTarget["cdn.example.com"].Kind != "http" {
			t.Fatalf("cdn.example.com.Kind = %q, want http", byTarget["cdn.example.com"].Kind)
		}
		if _, ok := byTarget["db.internal:5432"]; ok {
			t.Fatalf("byTarget содержит db.internal:5432 — порт не снят у server.address (сплит хоста)")
		}
		if dh := byTarget["db.internal"]; dh.Kind != "http" || dh.Calls != 2 {
			t.Fatalf("db.internal = {Kind:%q Calls:%d}, want {http 2} (server.address:port + url.full:port должны схлопнуться)", dh.Kind, dh.Calls)
		}
		if _, ok := byTarget["oracle"]; ok {
			t.Fatalf("byTarget contains oracle (leaked from projectID7)")
		}
		if _, ok := byTarget["http"]; ok {
			t.Fatalf("byTarget contains degenerate 'http' target")
		}
		if len(deps) != 14 {
			t.Fatalf("len(deps) = %d, want 14 (%+v)", len(deps), deps)
		}

		if pg.Reads != 1 || pg.Writes != 1 {
			t.Fatalf("postgresql = {Reads:%d Writes:%d}, want {1 1}", pg.Reads, pg.Writes)
		}
		if got := pg.Direction(); got != trace.DirectionBoth {
			t.Fatalf("postgresql.Direction() = %q, want %q", got, trace.DirectionBoth)
		}
		ro := byTarget["readonly-db"]
		if ro.Kind != "database" || ro.Calls != 4 {
			t.Fatalf("readonly-db = {Kind:%q Calls:%d}, want {database 4}", ro.Kind, ro.Calls)
		}
		if ro.Reads != 4 || ro.Writes != 0 {
			t.Fatalf("readonly-db = {Reads:%d Writes:%d}, want {4 0} (SELECT, WITH, select, «   SELECT»)", ro.Reads, ro.Writes)
		}
		if got := ro.Direction(); got != trace.DirectionIn {
			t.Fatalf("readonly-db.Direction() = %q, want %q", got, trace.DirectionIn)
		}
		sq := byTarget["sqlite"]
		if sq.Calls != 2 || sq.Reads != 0 || sq.Writes != 0 {
			t.Fatalf("sqlite = {Calls:%d Reads:%d Writes:%d}, want {2 0 0} (BEGIN и «1 SELECT» — ни чтение, ни запись)", sq.Calls, sq.Reads, sq.Writes)
		}
		if got := sq.Direction(); got != trace.DirectionNone {
			t.Fatalf("sqlite.Direction() = %q, want %q", got, trace.DirectionNone)
		}
		my := byTarget["mysql"]
		if my.Reads != 0 || my.Writes != 1 {
			t.Fatalf("mysql = {Reads:%d Writes:%d}, want {0 1} (db.operation.name=INSERT важнее description)", my.Reads, my.Writes)
		}
		if got := my.Direction(); got != trace.DirectionOut {
			t.Fatalf("mysql.Direction() = %q, want %q", got, trace.DirectionOut)
		}
		oo := byTarget["oldop-db"]
		if oo.Reads != 0 || oo.Writes != 1 || oo.Direction() != trace.DirectionOut {
			t.Fatalf("oldop-db = {Reads:%d Writes:%d Direction:%q}, want {0 1 out} (fallback на db.operation)", oo.Reads, oo.Writes, oo.Direction())
		}
		eo := byTarget["emptyop-db"]
		if eo.Reads != 1 || eo.Writes != 0 {
			t.Fatalf("emptyop-db = {Reads:%d Writes:%d}, want {1 0} (пустой атрибут → description)", eo.Reads, eo.Writes)
		}
		cd := byTarget["crossattr-db"]
		if cd.Reads != 1 || cd.Writes != 0 || cd.Direction() != trace.DirectionIn {
			t.Fatalf("crossattr-db = {Reads:%d Writes:%d Direction:%q}, want {1 0 in} (http-атрибут у db-спана не учитывается)", cd.Reads, cd.Writes, cd.Direction())
		}
		rd := byTarget["redis"]
		if rd.Calls != 2 || rd.Reads != 1 || rd.Writes != 1 {
			t.Fatalf("redis = {Calls:%d Reads:%d Writes:%d}, want {2 1 1}", rd.Calls, rd.Reads, rd.Writes)
		}
		if got := rd.Direction(); got != trace.DirectionBoth {
			t.Fatalf("redis.Direction() = %q, want %q", got, trace.DirectionBoth)
		}
		mc := byTarget["memcached"]
		if mc.Kind != "cache" || mc.Calls != 2 || mc.Reads != 1 || mc.Writes != 1 {
			t.Fatalf("memcached = {Kind:%q Calls:%d Reads:%d Writes:%d}, want {cache 2 1 1} (get + flush_all)", mc.Kind, mc.Calls, mc.Reads, mc.Writes)
		}
		if got := mc.Direction(); got != trace.DirectionBoth {
			t.Fatalf("memcached.Direction() = %q, want %q", got, trace.DirectionBoth)
		}
		st := byTarget["api.stripe.com"]
		if st.Reads != 0 || st.Writes != 1 {
			t.Fatalf("api.stripe.com = {Reads:%d Writes:%d}, want {0 1}", st.Reads, st.Writes)
		}
		if got := st.Direction(); got != trace.DirectionOut {
			t.Fatalf("api.stripe.com.Direction() = %q, want %q", got, trace.DirectionOut)
		}
		cdn := byTarget["cdn.example.com"]
		if cdn.Reads != 1 || cdn.Writes != 0 {
			t.Fatalf("cdn.example.com = {Reads:%d Writes:%d}, want {1 0}", cdn.Reads, cdn.Writes)
		}
		if got := cdn.Direction(); got != trace.DirectionIn {
			t.Fatalf("cdn.example.com.Direction() = %q, want %q", got, trace.DirectionIn)
		}
		lg := byTarget["legacy.example.com"]
		if lg.Calls != 1 || lg.Reads != 0 || lg.Writes != 1 || lg.Direction() != trace.DirectionOut {
			t.Fatalf("legacy.example.com = {Calls:%d Reads:%d Writes:%d Direction:%q}, want {1 0 1 out} (fallback на http.method)", lg.Calls, lg.Reads, lg.Writes, lg.Direction())
		}
		xh := byTarget["x.example.com"]
		if xh.Reads != 1 || xh.Writes != 0 || xh.Direction() != trace.DirectionIn {
			t.Fatalf("x.example.com = {Reads:%d Writes:%d Direction:%q}, want {1 0 in} (db-атрибут у http-спана не учитывается)", xh.Reads, xh.Writes, xh.Direction())
		}
		dh := byTarget["db.internal"]
		if dh.Reads != 0 || dh.Writes != 0 || dh.Direction() != trace.DirectionNone {
			t.Fatalf("db.internal = {Reads:%d Writes:%d Direction:%q}, want {0 0 %q}", dh.Reads, dh.Writes, dh.Direction(), trace.DirectionNone)
		}
	})

	t.Run("EndpointLatency5m", func(t *testing.T) {
		pts, err := q.EndpointLatency(ctx, projectID, "GET /api/users", from, to, 5*time.Minute, "production")
		if err != nil {
			t.Fatalf("EndpointLatency: %v", err)
		}
		if len(pts) != 12 {
			t.Fatalf("len(pts) = %d, want 12", len(pts))
		}
		for i := 1; i < len(pts); i++ {
			if !pts[i].T.After(pts[i-1].T) {
				t.Fatalf("points not chronological: %v", pts)
			}
		}
		if !pts[0].T.Equal(from) {
			t.Fatalf("pts[0].T = %v, want %v", pts[0].T, from)
		}
		var sum uint64
		for _, p := range pts {
			sum += p.Count
		}
		if sum != 100 {
			t.Fatalf("sum(Count) = %d, want 100", sum)
		}
		var seenP50 bool
		for _, p := range pts {
			if p.Count > 0 && p.P50 > 0 {
				seenP50 = true
			}
		}
		if !seenP50 {
			t.Fatalf("no bucket carried a p50: %v", pts)
		}
	})

	t.Run("EndpointLatencyRaw7m", func(t *testing.T) {
		pts, err := q.EndpointLatency(ctx, projectID, "GET /api/users", from, to, 7*time.Minute, "production")
		if err != nil {
			t.Fatalf("EndpointLatency 7m: %v", err)
		}
		if len(pts) == 0 {
			t.Fatalf("empty result")
		}
		var sum uint64
		for _, p := range pts {
			sum += p.Count
		}
		if sum != 100 {
			t.Fatalf("sum(Count) = %d, want 100 (all-zeros → grid misalignment)", sum)
		}
	})

	t.Run("EndpointLatencyBatch5m", func(t *testing.T) {
		names := []string{"GET /api/users", "GET /api/orders"}
		batch, err := q.EndpointLatencyBatch(ctx, projectID, names, from, to, 5*time.Minute, "production")
		if err != nil {
			t.Fatalf("EndpointLatencyBatch: %v", err)
		}
		if len(batch) != len(names) {
			t.Fatalf("len(batch) = %d, want %d (%+v)", len(batch), len(names), batch)
		}
		for _, name := range names {
			want, err := q.EndpointLatency(ctx, projectID, name, from, to, 5*time.Minute, "production")
			if err != nil {
				t.Fatalf("EndpointLatency(%q): %v", name, err)
			}
			got, ok := batch[name]
			if !ok {
				t.Fatalf("batch missing %q", name)
			}
			if len(got) != len(want) {
				t.Fatalf("%s: len(got) = %d, want %d", name, len(got), len(want))
			}
			for i := range want {
				if !got[i].T.Equal(want[i].T) || got[i].P50 != want[i].P50 ||
					got[i].P95 != want[i].P95 || got[i].Count != want[i].Count {
					t.Fatalf("%s point %d = %+v, want %+v", name, i, got[i], want[i])
				}
			}
		}
	})

	t.Run("EndpointLatencyBatchRaw7m", func(t *testing.T) {
		const name = "GET /api/users"
		batch, err := q.EndpointLatencyBatch(ctx, projectID, []string{name}, from, to, 7*time.Minute, "production")
		if err != nil {
			t.Fatalf("EndpointLatencyBatch raw: %v", err)
		}
		want, err := q.EndpointLatency(ctx, projectID, name, from, to, 7*time.Minute, "production")
		if err != nil {
			t.Fatalf("EndpointLatency raw: %v", err)
		}
		got := batch[name]
		if len(got) != len(want) {
			t.Fatalf("len(got) = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if !got[i].T.Equal(want[i].T) || got[i].P50 != want[i].P50 ||
				got[i].P95 != want[i].P95 || got[i].Count != want[i].Count {
				t.Fatalf("point %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("EndpointLatencyBatchEmpty", func(t *testing.T) {
		got, err := q.EndpointLatencyBatch(ctx, projectID, nil, from, to, 5*time.Minute, "production")
		if err != nil {
			t.Fatalf("EndpointLatencyBatch empty: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("len(got) = %d, want 0 (%+v)", len(got), got)
		}
	})

	t.Run("DurationHistogram", func(t *testing.T) {
		buckets := 10
		hist, err := q.DurationHistogram(ctx, projectID, "GET /api/users", from, to, "production", buckets)
		if err != nil {
			t.Fatalf("DurationHistogram: %v", err)
		}
		if len(hist) != buckets {
			t.Fatalf("len(hist) = %d, want %d", len(hist), buckets)
		}
		var sum uint64
		for i, b := range hist {
			sum += b.Count
			if i > 0 && b.UpperUS <= hist[i-1].UpperUS {
				t.Fatalf("UpperUS not increasing at %d: %v", i, hist)
			}
		}
		if sum != 100 {
			t.Fatalf("sum(Count) = %d, want 100", sum)
		}
	})

	t.Run("DurationHistogramEmpty", func(t *testing.T) {
		hist, err := q.DurationHistogram(ctx, projectID, "does not exist", from, to, "", 10)
		if err != nil {
			t.Fatalf("DurationHistogram empty: %v", err)
		}
		if hist != nil {
			t.Fatalf("hist = %v, want nil for no data", hist)
		}
	})

	t.Run("SlowestTraces", func(t *testing.T) {
		got, err := q.SlowestTraces(ctx, projectID, "GET /api/users", from, to, 5)
		if err != nil {
			t.Fatalf("SlowestTraces: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("len(got) = %d, want 5", len(got))
		}
		if got[0].DurationUS != 100000 {
			t.Fatalf("slowest DurationUS = %d, want 100000", got[0].DurationUS)
		}
		for i := 1; i < len(got); i++ {
			if got[i].DurationUS > got[i-1].DurationUS {
				t.Fatalf("not DESC by duration: %v", got)
			}
		}
		if got[0].TraceID == "" {
			t.Fatalf("slowest TraceID empty")
		}
	})

	t.Run("OffendingSpans", func(t *testing.T) {
		spans, err := q.OffendingSpans(ctx, projectID5, "off-trace", []string{"offdb1", "missing"})
		if err != nil {
			t.Fatalf("OffendingSpans: %v", err)
		}
		if len(spans) != 1 {
			t.Fatalf("len(spans) = %d, want 1", len(spans))
		}
		s := spans[0]
		if s.Description != "SELECT * FROM payments JOIN ledger ON ledger.id = payments.id" {
			t.Errorf("full description not returned: %q", s.Description)
		}
		if s.Op != "db.sql.query" || s.DurationUS != 900000 {
			t.Errorf("op/duration = %q %d", s.Op, s.DurationUS)
		}
		if s.Data["db.system"] != "postgresql" || s.Data["code.filepath"] != "app/pay.py" {
			t.Errorf("data not decoded: %v", s.Data)
		}
		if s.Data["code.lineno"] != "42" {
			t.Errorf("numeric data value not decoded to text: %q", s.Data["code.lineno"])
		}
		if got, err := q.OffendingSpans(ctx, projectID5, "off-trace", nil); err != nil || got != nil {
			t.Errorf("empty ids → want nil,nil; got %v %v", got, err)
		}
		if got, err := q.OffendingSpans(ctx, projectID5, "no-such-trace", []string{"x"}); err != nil || got != nil {
			t.Errorf("missing trace → want nil,nil; got %v %v", got, err)
		}
	})

	t.Run("Trace", func(t *testing.T) {
		root, spans, err := q.Trace(ctx, projectID, wfTrace)
		if err != nil {
			t.Fatalf("Trace: %v", err)
		}
		if len(spans) != 3 {
			t.Fatalf("len(spans) = %d, want 3", len(spans))
		}
		if root.TraceID != wfTrace || root.DurationUS != 300000 || root.Status != "ok" {
			t.Fatalf("root = %+v", root)
		}
		if !root.Timestamp.Equal(wfStart) {
			t.Fatalf("root.Timestamp = %v, want %v", root.Timestamp, wfStart)
		}
		byID := make(map[string]trace.SpanRow, len(spans))
		for _, s := range spans {
			byID[s.SpanID] = s
		}
		if r, ok := byID["wf-root"]; !ok || r.StartUS != 0 || r.ParentSpanID != "" {
			t.Fatalf("root span = %+v (ok=%v)", r, ok)
		}
		if db, ok := byID["wf-db"]; !ok || db.StartUS != 20000 || db.DurationUS != 60000 {
			t.Fatalf("wf-db span = %+v (ok=%v)", db, ok)
		}
		if h, ok := byID["wf-http"]; !ok || h.StartUS != 90000 || h.Description != "GET https://x/y" {
			t.Fatalf("wf-http span = %+v (ok=%v)", h, ok)
		}
	})

	t.Run("TraceNotFound", func(t *testing.T) {
		root, spans, err := q.Trace(ctx, projectID, "no-such-trace")
		if err != nil {
			t.Fatalf("Trace not found: %v", err)
		}
		if spans != nil || root.TraceID != "" {
			t.Fatalf("want empty result, got root=%+v spans=%v", root, spans)
		}
	})

	t.Run("ProjectForTrace", func(t *testing.T) {
		pid, found, err := q.ProjectForTrace(ctx, wfTrace)
		if err != nil {
			t.Fatalf("ProjectForTrace: %v", err)
		}
		if !found || pid != projectID {
			t.Fatalf("ProjectForTrace = (%d, %v), want (%d, true)", pid, found, projectID)
		}

		_, found, err = q.ProjectForTrace(ctx, "unknown-trace-id")
		if err != nil {
			t.Fatalf("ProjectForTrace unknown: %v", err)
		}
		if found {
			t.Fatalf("found = true for unknown trace")
		}
	})

	t.Run("EndpointLatencyP95Index", func(t *testing.T) {
		pts, err := q.EndpointLatency(ctx, projectID2, "GET /lat", from, to, 5*time.Minute, "production")
		if err != nil {
			t.Fatalf("EndpointLatency lat: %v", err)
		}
		var filled *trace.LatencyPoint
		for i := range pts {
			if pts[i].Count == 100 {
				filled = &pts[i]
				break
			}
		}
		if filled == nil {
			t.Fatalf("no bucket with all 100 transactions: %+v", pts)
		}
		assertNear(t, "lat p50", filled.P50, 50500, 2)
		assertNear(t, "lat p95", filled.P95, 95050, 2)
	})

	t.Run("ApdexBoundary", func(t *testing.T) {
		got, _, err := q.Endpoints(ctx, projectID2, from, to, "production", 50)
		if err != nil {
			t.Fatalf("Endpoints apdex: %v", err)
		}
		var apdex float64
		var found bool
		for _, e := range got {
			if e.Transaction == "GET /apdex" {
				apdex = e.ApdexScore
				found = true
			}
		}
		if !found {
			t.Fatalf("GET /apdex not in endpoints: %+v", got)
		}
		if apdex < 0.249 || apdex > 0.251 {
			t.Fatalf("apdex = %v, want 0.25 (4T boundary must be inclusive)", apdex)
		}
	})

	t.Run("TraceLimit", func(t *testing.T) {
		_, spans, err := q.Trace(ctx, projectID2, bigTrace)
		if err != nil {
			t.Fatalf("Trace big: %v", err)
		}
		if len(spans) != 5000 {
			t.Fatalf("len(spans) = %d, want 5000 (LIMIT)", len(spans))
		}
	})

	t.Run("WebVitalsPages", func(t *testing.T) {
		pages, err := q.WebVitalsPages(ctx, projectID3, from, to, "production")
		if err != nil {
			t.Fatalf("WebVitalsPages: %v", err)
		}
		if len(pages) != 2 {
			t.Fatalf("len(pages) = %d, want 2 (%+v)", len(pages), pages)
		}
		for _, p := range pages {
			if p.Transaction == "GET /api/noop" {
				t.Fatalf("WebVitalsPages returned empty page %q (want filtered out): %+v", p.Transaction, p)
			}
		}
		if pages[0].Transaction != "GET /slow" || pages[1].Transaction != "GET /home" {
			t.Fatalf("order: %q, %q", pages[0].Transaction, pages[1].Transaction)
		}

		slow := pages[0]
		if slow.LCP.Count != 5 {
			t.Fatalf("slow.LCP.Count = %d, want 5", slow.LCP.Count)
		}
		assertNearF(t, "slow lcp p75", slow.LCP.P75, 5000, 1)
		if slow.LCP.Rating != "poor" {
			t.Fatalf("slow.LCP.Rating = %q, want poor", slow.LCP.Rating)
		}

		home := pages[1]
		if home.LCP.Count != 3 {
			t.Fatalf("home.LCP.Count = %d, want 3", home.LCP.Count)
		}
		if home.Count != 3 {
			t.Fatalf("home.Count = %d, want 3", home.Count)
		}
		assertNearF(t, "home lcp p75", home.LCP.P75, 2500, 60)
		if home.LCP.Rating != trace.Rating("lcp", home.LCP.P75) {
			t.Fatalf("home.LCP.Rating = %q inconsistent with Rating(%v)", home.LCP.Rating, home.LCP.P75)
		}
		if home.CLS.Count != 3 || home.CLS.Rating != "good" {
			t.Fatalf("home.CLS = %+v, want count 3 rating good", home.CLS)
		}
		assertNearF(t, "home cls p75", home.CLS.P75, 0.05, 0.001)
		if home.INP.Count != 0 || home.INP.Rating != "" {
			t.Fatalf("home.INP = %+v, want count 0 rating \"\"", home.INP)
		}
	})

	t.Run("WebVitalsPagesEnvironmentFilter", func(t *testing.T) {
		all, err := q.WebVitalsPages(ctx, projectID3, from, to, "")
		if err != nil {
			t.Fatalf("WebVitalsPages all: %v", err)
		}
		var homeAll uint64
		for _, p := range all {
			if p.Transaction == "GET /home" {
				homeAll = p.LCP.Count
			}
		}
		if homeAll != 5 {
			t.Fatalf("home lcp count without env filter = %d, want 5", homeAll)
		}
	})

	t.Run("WebVitalsPagesEmpty", func(t *testing.T) {
		pages, err := q.WebVitalsPages(ctx, 999999, from, to, "")
		if err != nil {
			t.Fatalf("WebVitalsPages empty: %v", err)
		}
		if len(pages) != 0 {
			t.Fatalf("len(pages) = %d, want 0 for unknown project", len(pages))
		}
	})

	t.Run("VitalSeries", func(t *testing.T) {
		pts, err := q.VitalSeries(ctx, projectID3, "GET /home", "lcp", from, to, 5*time.Minute, "production")
		if err != nil {
			t.Fatalf("VitalSeries: %v", err)
		}
		if len(pts) != 1 {
			t.Fatalf("len(pts) = %d, want 1 (%+v)", len(pts), pts)
		}
		if !pts[0].T.Equal(from) {
			t.Fatalf("pts[0].T = %v, want %v", pts[0].T, from)
		}
		assertNearF(t, "series lcp p75", pts[0].P75, 2500, 60)
	})

	t.Run("VitalSeriesUnknownName", func(t *testing.T) {
		if _, err := q.VitalSeries(ctx, projectID3, "GET /home", "bogus", from, to, 5*time.Minute, ""); err == nil {
			t.Fatalf("VitalSeries with unknown vital name: want error, got nil")
		}
	})

	t.Run("PageVitalsOne", func(t *testing.T) {
		lcp, inp, cls, fcp, ttfb, err := q.PageVitalsOne(ctx, projectID3, "GET /home", from, to, "production")
		if err != nil {
			t.Fatalf("PageVitalsOne home: %v", err)
		}
		if lcp.Count != 3 {
			t.Fatalf("home lcp count = %d, want 3", lcp.Count)
		}
		assertNearF(t, "home lcp p75", lcp.P75, 2500, 60)
		if lcp.Rating != trace.Rating("lcp", lcp.P75) {
			t.Fatalf("home lcp rating %q inconsistent with Rating(%v)", lcp.Rating, lcp.P75)
		}
		if cls.Count != 3 || cls.Rating != "good" {
			t.Fatalf("home cls = %+v, want count 3 rating good", cls)
		}
		assertNearF(t, "home cls p75", cls.P75, 0.05, 0.001)
		for _, v := range []trace.Vital{inp, fcp, ttfb} {
			if v.Count != 0 || v.Rating != "" {
				t.Fatalf("%s = %+v, want count 0 rating \"\"", v.Name, v)
			}
		}
	})

	t.Run("PageVitalsOneEnvironmentFilter", func(t *testing.T) {
		lcp, _, _, _, _, err := q.PageVitalsOne(ctx, projectID3, "GET /home", from, to, "staging")
		if err != nil {
			t.Fatalf("PageVitalsOne staging: %v", err)
		}
		if lcp.Count != 2 {
			t.Fatalf("home lcp count (staging) = %d, want 2", lcp.Count)
		}
		assertNearF(t, "home lcp p75 staging", lcp.P75, 9000, 1)
		if lcp.Rating != "poor" {
			t.Fatalf("home lcp rating (staging) = %q, want poor", lcp.Rating)
		}
		lcpAll, _, _, _, _, err := q.PageVitalsOne(ctx, projectID3, "GET /home", from, to, "")
		if err != nil {
			t.Fatalf("PageVitalsOne all env: %v", err)
		}
		if lcpAll.Count != 5 {
			t.Fatalf("home lcp count (all env) = %d, want 5", lcpAll.Count)
		}
	})

	t.Run("PageVitalsOneNoVitals", func(t *testing.T) {
		vs, err := func() ([]trace.Vital, error) {
			lcp, inp, cls, fcp, ttfb, err := q.PageVitalsOne(ctx, projectID, "GET /api/users", from, to, "production")
			return []trace.Vital{lcp, inp, cls, fcp, ttfb}, err
		}()
		if err != nil {
			t.Fatalf("PageVitalsOne users: %v", err)
		}
		for _, v := range vs {
			if v.Count != 0 || v.Rating != "" {
				t.Fatalf("users %s = %+v, want count 0 rating \"\" (no measurements)", v.Name, v)
			}
		}
	})

	t.Run("RecentVitalP75", func(t *testing.T) {
		s, err := q.RecentVitalP75(ctx, projectID4, "GET /vpage", "lcp", regRecentFrom, regRecentTo)
		if err != nil {
			t.Fatalf("RecentVitalP75: %v", err)
		}
		if s.Samples != 30 {
			t.Fatalf("recent vital samples = %d, want 30", s.Samples)
		}
		assertNearF(t, "recent lcp p75", s.Value, 2000, 1)
	})

	t.Run("BaselineVitalP75", func(t *testing.T) {
		s, err := q.BaselineVitalP75(ctx, projectID4, "GET /vpage", "lcp", 7, regNow)
		if err != nil {
			t.Fatalf("BaselineVitalP75: %v", err)
		}
		if s.Samples != 70 {
			t.Fatalf("baseline vital samples = %d, want 70", s.Samples)
		}
		assertNearF(t, "baseline median lcp p75", s.Value, 800, 1)
	})

	t.Run("VitalP75UnknownName", func(t *testing.T) {
		if _, err := q.RecentVitalP75(ctx, projectID4, "GET /vpage", "bogus", regRecentFrom, regRecentTo); err == nil {
			t.Fatalf("RecentVitalP75 unknown name: want error, got nil")
		}
		if _, err := q.BaselineVitalP75(ctx, projectID4, "GET /vpage", "bogus", 7, regNow); err == nil {
			t.Fatalf("BaselineVitalP75 unknown name: want error, got nil")
		}
	})

	t.Run("RecentEndpointP95sConvertsToMs", func(t *testing.T) {
		out, err := q.RecentEndpointP95s(ctx, projectID4, []string{"GET /reg"}, regRecentFrom, regRecentTo)
		if err != nil {
			t.Fatalf("RecentEndpointP95s: %v", err)
		}
		s, ok := out["GET /reg"]
		if !ok {
			t.Fatalf("RecentEndpointP95s: нет ключа %q в %v", "GET /reg", out)
		}
		if s.Samples != 50 {
			t.Fatalf("recent samples = %d, want 50", s.Samples)
		}
		assertNearF(t, "recent p95 ms (must be 1000, not 1_000_000 µs)", s.Value, 1000, 1)
	})

	t.Run("BaselineEndpointP95sConvertsToMs", func(t *testing.T) {
		out, err := q.BaselineEndpointP95s(ctx, projectID4, []string{"GET /reg"}, 7, regNow)
		if err != nil {
			t.Fatalf("BaselineEndpointP95s: %v", err)
		}
		s, ok := out["GET /reg"]
		if !ok {
			t.Fatalf("BaselineEndpointP95s: нет ключа %q в %v", "GET /reg", out)
		}
		if s.Samples != 130 {
			t.Fatalf("baseline samples = %d, want 130", s.Samples)
		}
		assertNearF(t, "baseline median p95 ms (must be 300, not 300_000 µs)", s.Value, 300, 1)
	})

	t.Run("TopEndpointsByTraffic", func(t *testing.T) {
		top, err := q.TopEndpointsByTraffic(ctx, projectID4, regNow.Add(-7*24*time.Hour), regNow, 2)
		if err != nil {
			t.Fatalf("TopEndpointsByTraffic: %v", err)
		}
		if len(top) != 2 {
			t.Fatalf("len(top) = %d, want 2 (LIMIT)", len(top))
		}
		if top[0] != "GET /reg" {
			t.Fatalf("top[0] = %q, want GET /reg (highest traffic)", top[0])
		}
		if k0, err := q.TopEndpointsByTraffic(ctx, projectID4, regRecentFrom, regRecentTo, 0); err != nil || k0 != nil {
			t.Fatalf("TopEndpointsByTraffic k=0 = (%v, %v), want (nil, nil)", k0, err)
		}
	})

	t.Run("TopVitalPages", func(t *testing.T) {
		top, err := q.TopVitalPages(ctx, projectID4, regNow.Add(-7*24*time.Hour), regNow, 10)
		if err != nil {
			t.Fatalf("TopVitalPages: %v", err)
		}
		if len(top) != 2 {
			t.Fatalf("len(top) = %d, want 2 (%v)", len(top), top)
		}
		if top[0] != "GET /vpage" || top[1] != "GET /vpage2" {
			t.Fatalf("top = %v, want [GET /vpage GET /vpage2]", top)
		}
	})
}

func TestDependencyDirection(t *testing.T) {
	cases := []struct {
		name   string
		reads  int64
		writes int64
		want   trace.DataDirection
	}{
		{"none", 0, 0, trace.DirectionNone},
		{"in", 3, 0, trace.DirectionIn},
		{"out", 0, 2, trace.DirectionOut},
		{"both", 1, 1, trace.DirectionBoth},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := trace.Dependency{Reads: c.reads, Writes: c.writes}
			if got := d.Direction(); got != c.want {
				t.Fatalf("Direction(reads=%d, writes=%d) = %q, want %q", c.reads, c.writes, got, c.want)
			}
		})
	}
	// Значения констант — контракт для UI/шаблонов (data-атрибуты, i18n-ключи).
	if trace.DirectionNone != "" || trace.DirectionIn != "in" || trace.DirectionOut != "out" || trace.DirectionBoth != "both" {
		t.Fatalf("DataDirection constants = %q/%q/%q/%q, want \"\"/in/out/both",
			trace.DirectionNone, trace.DirectionIn, trace.DirectionOut, trace.DirectionBoth)
	}
}

func TestVerbClasses(t *testing.T) {
	classes := []struct {
		kind        string
		read, write string
		wantR       string
		wantW       string
	}{
		{"database", trace.SQLReadVerbs, trace.SQLWriteVerbs, "SELECT", "INSERT"},
		{"cache", trace.CacheReadVerbs, trace.CacheWriteVerbs, "HGET", "SET"},
		{"http", trace.HTTPReadVerbs, trace.HTTPWriteVerbs, "GET", "POST"},
	}
	// verbList не экранирует, а в SQL глагол сравнивается после upper(): любой
	// символ вне [A-Z_] в списке — либо сломанный литерал, либо вечное несовпадение.
	verbRe := regexp.MustCompile(`^[A-Z_]+$`)
	for _, c := range classes {
		r := verbSet(c.read)
		w := verbSet(c.write)
		for _, set := range []map[string]bool{r, w} {
			for v := range set {
				if !verbRe.MatchString(v) {
					t.Fatalf("%s: глагол %q не матчит ^[A-Z_]+$", c.kind, v)
				}
			}
		}
		if !r[c.wantR] {
			t.Fatalf("%s: read verbs не содержат %s: %q", c.kind, c.wantR, c.read)
		}
		if !w[c.wantW] {
			t.Fatalf("%s: write verbs не содержат %s: %q", c.kind, c.wantW, c.write)
		}
		for v := range r {
			if w[v] {
				t.Fatalf("%s: глагол %s одновременно read и write", c.kind, v)
			}
		}
	}
}

func verbSet(list string) map[string]bool {
	set := map[string]bool{}
	for _, v := range strings.Fields(list) {
		set[v] = true
	}
	return set
}

func TestRating(t *testing.T) {
	cases := []struct {
		name string
		p75  float64
		want string
	}{
		{"lcp", 2500, "good"},
		{"lcp", 2501, "needs-improvement"},
		{"lcp", 4000, "needs-improvement"},
		{"lcp", 4001, "poor"},
		{"lcp", 0, "good"},
		{"inp", 200, "good"},
		{"inp", 201, "needs-improvement"},
		{"inp", 500, "needs-improvement"},
		{"inp", 501, "poor"},
		{"cls", 0.1, "good"},
		{"cls", 0.11, "needs-improvement"},
		{"cls", 0.25, "needs-improvement"},
		{"cls", 0.26, "poor"},
		{"fcp", 1800, "good"},
		{"fcp", 1801, "needs-improvement"},
		{"fcp", 3000, "needs-improvement"},
		{"fcp", 3001, "poor"},
		{"ttfb", 800, "good"},
		{"ttfb", 801, "needs-improvement"},
		{"ttfb", 1800, "needs-improvement"},
		{"ttfb", 1801, "poor"},
		{"unknown", 1, ""},
	}
	for _, c := range cases {
		if got := trace.Rating(c.name, c.p75); got != c.want {
			t.Errorf("Rating(%q, %v) = %q, want %q", c.name, c.p75, got, c.want)
		}
	}
}

func assertNearF(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	d := got - want
	if d < 0 {
		d = -d
	}
	if d > tol {
		t.Fatalf("%s = %v, want ~%v (±%v)", name, got, want, tol)
	}
}

func assertNear(t *testing.T, name string, got, want, tol uint32) {
	t.Helper()
	var d uint32
	if got > want {
		d = got - want
	} else {
		d = want - got
	}
	if d > tol {
		t.Fatalf("%s = %d, want ~%d (±%d)", name, got, want, tol)
	}
}
