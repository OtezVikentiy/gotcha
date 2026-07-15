package trace_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestQueryReadsFromClickHouse поднимает один CH-контейнер, наполняет его через
// SpanWriter и прогоняет все методы trace.Query подтестами (как
// event/uptime query_test): контейнер дорогой, поэтому один на всё.
func TestQueryReadsFromClickHouse(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const projectID = int64(55)
	const projectID2 = int64(56) // отдельный проект для тестов LIMIT/индексов

	w := trace.NewSpanWriter(conn)
	go w.Run()

	// Окно [base, base+1h) в прошлом, base выровнен по часу (кратен 5 минутам).
	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	from := base
	to := base.Add(time.Hour)

	// «GET /api/users», production: 100 транзакций, длительности 1000..100000 µs
	// (i+1)·1000, разнесённые по часу (по одной каждые 36 c). Первые 10 —
	// со статусом internal_error (failure rate = 0.10).
	for i := 0; i < 100; i++ {
		status := "ok"
		if i < 10 {
			status = "internal_error"
		}
		at := base.Add(time.Duration(i) * 36 * time.Second)
		dur := time.Duration(i+1) * 1000 * time.Microsecond
		w.Add(projectID, trace.Transaction{
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

	// «GET /api/users», staging: 5 транзакций (короткие, 1000 µs) — для
	// проверки фильтра по окружению. Длительность мелкая нарочно, чтобы они не
	// вклинивались в SlowestTraces (у которого фильтра по окружению нет).
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		w.Add(projectID, trace.Transaction{
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

	// «GET /api/orders», production: 20 транзакций (2000 µs) — второй эндпойнт
	// в списке.
	for i := 0; i < 20; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		w.Add(projectID, trace.Transaction{
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

	// Отдельный трейс с дочерними спанами — для Trace/ProjectForTrace.
	const wfTrace = "waterfall-trace-id"
	wfStart := base.Add(10 * time.Minute)
	w.Add(projectID, trace.Transaction{
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

	// projectID2 «GET /lat»: 100 транзакций, все в первой 5-минутной корзине
	// [base, base+5m), длительности (i+1)·1000 µs (1000..100000). База выровнена
	// по часу, значит и по 5м, поэтому вся сотня попадает в одну корзину — тогда
	// её p50/p95 совпадают с общими и можно проверить точный ИНДЕКС перцентиля
	// в EndpointLatency (перепутанный qs[1]/qs[2] дал бы p75 вместо p95).
	for i := 0; i < 100; i++ {
		at := base.Add(time.Duration(i) * 3 * time.Second) // 0..297 c → корзина 0
		dur := time.Duration(i+1) * 1000 * time.Microsecond
		w.Add(projectID2, trace.Transaction{
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

	// projectID2 «GET /apdex»: длительности ровно на границе 4T (при T=50мс это
	// 200000 µs). Две транзакции ровно 200000 µs (== 4T) должны попасть в
	// tolerating (граница `<=` включительна), две по 200001 µs (> 4T) — нет.
	// Ни одна не satisfied (все > T·1000 = 50000). Apdex = (0 + 2)/(2·4) = 0.25.
	for i := 0; i < 2; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID2, trace.Transaction{
			TraceID:     fmt.Sprintf("apdex-on-%d", i),
			SpanID:      fmt.Sprintf("apdexspan-on-%d", i),
			Name:        "GET /apdex",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(200 * time.Millisecond), // ровно 200000 µs == 4T
			Environment: "production",
		})
	}
	for i := 0; i < 2; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID2, trace.Transaction{
			TraceID:     fmt.Sprintf("apdex-over-%d", i),
			SpanID:      fmt.Sprintf("apdexspan-over-%d", i),
			Name:        "GET /apdex",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(200*time.Millisecond + time.Microsecond), // 200001 µs > 4T
			Environment: "production",
		})
	}

	// projectID2 «GET /big»: трейс с 5100 дочерними спанами (> лимита Trace),
	// чтобы проверить, что Trace ограничивает число прочитанных строк.
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
	w.Add(projectID2, trace.Transaction{
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

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	q := trace.NewQuery(conn)

	t.Run("Endpoints", func(t *testing.T) {
		got, err := q.Endpoints(ctx, projectID, from, to, "production", 50)
		if err != nil {
			t.Fatalf("Endpoints: %v", err)
		}
		// production: users(100), orders(20), checkout(1 — из waterfall-трейса).
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3 (%+v)", len(got), got)
		}
		// ORDER BY count DESC.
		if got[0].Transaction != "GET /api/users" || got[1].Transaction != "GET /api/orders" ||
			got[2].Transaction != "GET /api/checkout" {
			t.Fatalf("order: %q, %q, %q", got[0].Transaction, got[1].Transaction, got[2].Transaction)
		}
		users := got[0]
		if users.Count != 100 {
			t.Fatalf("users.Count = %d, want 100", users.Count)
		}
		// Throughput = 100 / 60 мин ≈ 1.667.
		if users.Throughput < 1.66 || users.Throughput > 1.67 {
			t.Fatalf("users.Throughput = %v, want ~1.667", users.Throughput)
		}
		// FailureRate = 10/100 = 0.10.
		if users.FailureRate < 0.099 || users.FailureRate > 0.101 {
			t.Fatalf("users.FailureRate = %v, want 0.10", users.FailureRate)
		}
		// Перцентили: линейная интерполяция ClickHouse по 1000..100000.
		// p50=50500, p75=75250, p95=95050, p99=99010.
		assertNear(t, "p50", users.P50, 50500, 2)
		assertNear(t, "p75", users.P75, 75250, 2)
		assertNear(t, "p95", users.P95, 95050, 2)
		assertNear(t, "p99", users.P99, 99010, 2)
		// Apdex T=50мс: satisfied=50 (dur≤50000), within4T=100 (dur≤200000).
		// (50+100)/(2·100) = 0.75.
		if users.ApdexScore < 0.749 || users.ApdexScore > 0.751 {
			t.Fatalf("users.ApdexScore = %v, want 0.75", users.ApdexScore)
		}
	})

	t.Run("EndpointsEnvironmentFilter", func(t *testing.T) {
		stg, err := q.Endpoints(ctx, projectID, from, to, "staging", 50)
		if err != nil {
			t.Fatalf("Endpoints staging: %v", err)
		}
		if len(stg) != 1 || stg[0].Transaction != "GET /api/users" || stg[0].Count != 5 {
			t.Fatalf("staging endpoints = %+v, want single users with count 5", stg)
		}

		all, err := q.Endpoints(ctx, projectID, from, to, "", 50)
		if err != nil {
			t.Fatalf("Endpoints all: %v", err)
		}
		var usersAll uint64
		for _, e := range all {
			if e.Transaction == "GET /api/users" {
				usersAll = e.Count
			}
		}
		if usersAll != 105 { // 100 production + 5 staging
			t.Fatalf("users count without env filter = %d, want 105", usersAll)
		}
	})

	t.Run("EndpointsEmptyProject", func(t *testing.T) {
		got, err := q.Endpoints(ctx, 999999, from, to, "", 50)
		if err != nil {
			t.Fatalf("Endpoints empty: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("len(got) = %d, want 0 for unknown project", len(got))
		}
	})

	t.Run("EndpointLatency5m", func(t *testing.T) {
		pts, err := q.EndpointLatency(ctx, projectID, "GET /api/users", from, to, 5*time.Minute, "production")
		if err != nil {
			t.Fatalf("EndpointLatency: %v", err)
		}
		// Окно час, шаг 5м, выровнено по epoch → 12 интервалов + граничная точка.
		if len(pts) != 13 {
			t.Fatalf("len(pts) = %d, want 13", len(pts))
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
		// Хотя бы в одной непустой корзине перцентили заполнены.
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
		// 7м не кратно 5м → чтение из сырых transactions, epoch-выравнивание.
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

	t.Run("Trace", func(t *testing.T) {
		root, spans, err := q.Trace(ctx, projectID, wfTrace)
		if err != nil {
			t.Fatalf("Trace: %v", err)
		}
		if len(spans) != 3 { // корень + 2 дочерних
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
		// wf-db стартует на 20мс позже корня → 20000 µs.
		if db, ok := byID["wf-db"]; !ok || db.StartUS != 20000 || db.DurationUS != 60000 {
			t.Fatalf("wf-db span = %+v (ok=%v)", db, ok)
		}
		// wf-http стартует на 90мс позже корня → 90000 µs.
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
		// Вся сотня «GET /lat» — в первой 5м-корзине, поэтому её p50/p95 равны
		// общим по эндпойнту (см. Endpoints: p50=50500, p95=95050). Проверяем
		// именно p95, чтобы поймать перепутанный индекс перцентиля (qs[1] дал бы
		// p75≈75250).
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
		// Граница 4T включительна: две транзакции ровно на 200000 µs (== 4T)
		// tolerating, две по 200001 µs — frustrated. Apdex = (0+2)/(2·4) = 0.25.
		got, err := q.Endpoints(ctx, projectID2, from, to, "production", 50)
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
		// Трейс с 5100 спанами; Trace ограничивает выборку traceSpanLimit=5000.
		_, spans, err := q.Trace(ctx, projectID2, bigTrace)
		if err != nil {
			t.Fatalf("Trace big: %v", err)
		}
		if len(spans) != 5000 {
			t.Fatalf("len(spans) = %d, want 5000 (LIMIT)", len(spans))
		}
	})
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
