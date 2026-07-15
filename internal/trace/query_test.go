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
	const projectID3 = int64(57) // отдельный проект для Web Vitals
	const projectID4 = int64(58) // отдельный проект для запросов регрессий (данные по дням)

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

	// projectID3: Web Vitals. «GET /home» production — 3 pageload-транзакции с
	// lcp 2000/2400/2600 (p75≈2500, граница good) и cls 0.05, без inp; все в
	// первой 5м-корзине [base, base+5m).
	for i, lcp := range []float64{2000, 2400, 2600} {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID3, trace.Transaction{
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
	// «GET /slow» production — 5 транзакций lcp 5000 (p75=5000 → poor). Замеров
	// больше, чем у /home, поэтому идёт первой при сортировке по lcp_count DESC.
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID3, trace.Transaction{
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
	// «GET /home» staging — 2 транзакции lcp 9000, для проверки фильтра по
	// окружению (production lcp_count=3, без фильтра — 5).
	for i := 0; i < 2; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID3, trace.Transaction{
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

	// «GET /api/noop» production — 4 http.server-транзакции БЕЗ measurements:
	// MV web_vitals_5m их всё равно агрегирует (все *_count = 0), и без HAVING
	// они бы засоряли список WebVitalsPages пустыми строками.
	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID3, trace.Transaction{
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

	// projectID4: данные для запросов детектора регрессий, разнесённые по дням.
	// Якорь regNow — полдень вчерашнего дня (UTC): заведомо в прошлом и вдали от
	// полуночи, поэтому toStartOfDay кладёт свежее окно и прошлые дни в разные
	// календарные сутки без риска пограничного дребезга.
	regNow := time.Now().UTC().Truncate(24 * time.Hour).Add(-12 * time.Hour)
	regRecentFrom := regNow.Add(-time.Hour)
	regRecentTo := regNow
	regRecentAt := regNow.Add(-30 * time.Minute) // внутри свежего окна [from,to)

	// Эндпойнт «GET /reg»: сегодня (свежее окно) 50 транзакций по 1000 мс →
	// recent p95 = 1000 мс; день-1 40 по 200 мс; день-2 40 по 300 мс. Дневные
	// p95 = [1000, 200, 300], медиана = 300 мс; всего замеров 130.
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
			w.Add(projectID4, trace.Transaction{
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

	// Страница «GET /vpage» с lcp: сегодня 30 замеров lcp 2000 → recent p75 =
	// 2000; день-1 20 по 500; день-2 20 по 800. Дневные p75 = [2000, 500, 800],
	// медиана = 800; всего замеров 70.
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
			w.Add(projectID4, trace.Transaction{
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

	// Страница «GET /vpage2» с 5 замерами lcp сегодня — низкотрафичная, для
	// проверки ранжирования TopVitalPages (её меньше, чем у /vpage).
	for i := 0; i < 5; i++ {
		w.Add(projectID4, trace.Transaction{
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

	t.Run("WebVitalsPages", func(t *testing.T) {
		pages, err := q.WebVitalsPages(ctx, projectID3, from, to, "production")
		if err != nil {
			t.Fatalf("WebVitalsPages: %v", err)
		}
		if len(pages) != 2 {
			t.Fatalf("len(pages) = %d, want 2 (%+v)", len(pages), pages)
		}
		// HAVING (lcp+inp+cls > 0) отсекает «GET /api/noop» без замеров: в списке
		// только страницы, у которых реально есть хоть один LCP/INP/CLS.
		for _, p := range pages {
			if p.Transaction == "GET /api/noop" {
				t.Fatalf("WebVitalsPages returned empty page %q (want filtered out): %+v", p.Transaction, p)
			}
		}
		// ORDER BY lcp_count DESC → /slow (5 замеров) перед /home (3).
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
		// p75([2000,2400,2600]) = 2500 (линейная интерполяция).
		assertNearF(t, "home lcp p75", home.LCP.P75, 2500, 60)
		// Рейтинг должен быть согласован с чистой Rating() на фактическом p75.
		if home.LCP.Rating != trace.Rating("lcp", home.LCP.P75) {
			t.Fatalf("home.LCP.Rating = %q inconsistent with Rating(%v)", home.LCP.Rating, home.LCP.P75)
		}
		// cls 0.05 → good.
		if home.CLS.Count != 3 || home.CLS.Rating != "good" {
			t.Fatalf("home.CLS = %+v, want count 3 rating good", home.CLS)
		}
		assertNearF(t, "home cls p75", home.CLS.P75, 0.05, 0.001)
		// Без inp → Count 0 и Rating "" (нет данных, не «good»).
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
		if homeAll != 5 { // 3 production + 2 staging
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
		// Все три замера в одной 5м-корзине → одна точка ряда с p75≈2500.
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
		// GET /home production: lcp count 3 (p75≈2500), cls count 3 (good), без
		// inp/fcp/ttfb → Count 0 и Rating "".
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
		// Отсутствующие vitals — Count 0, Rating "" (нет данных, не «good»).
		for _, v := range []trace.Vital{inp, fcp, ttfb} {
			if v.Count != 0 || v.Rating != "" {
				t.Fatalf("%s = %+v, want count 0 rating \"\"", v.Name, v)
			}
		}
	})

	t.Run("PageVitalsOneEnvironmentFilter", func(t *testing.T) {
		// staging: только 2 транзакции lcp 9000 (poor). production сюда не входит.
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
		// Без фильтра окружения — 5 замеров (3 production + 2 staging).
		lcpAll, _, _, _, _, err := q.PageVitalsOne(ctx, projectID3, "GET /home", from, to, "")
		if err != nil {
			t.Fatalf("PageVitalsOne all env: %v", err)
		}
		if lcpAll.Count != 5 {
			t.Fatalf("home lcp count (all env) = %d, want 5", lcpAll.Count)
		}
	})

	t.Run("PageVitalsOneNoVitals", func(t *testing.T) {
		// Транзакция без measurements → все пять Count 0 и Rating "" (панель на
		// вебе по этому и не рендерится).
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

	t.Run("RecentEndpointP95", func(t *testing.T) {
		// Свежее окно: 50 транзакций по 1000 мс → p95 = 1000 мс (в мс, не µs).
		s, err := q.RecentEndpointP95(ctx, projectID4, "GET /reg", regRecentFrom, regRecentTo)
		if err != nil {
			t.Fatalf("RecentEndpointP95: %v", err)
		}
		if s.Samples != 50 {
			t.Fatalf("recent samples = %d, want 50", s.Samples)
		}
		assertNearF(t, "recent p95 ms", s.Value, 1000, 1)
	})

	t.Run("BaselineEndpointP95", func(t *testing.T) {
		// Дневные p95 = [1000, 200, 300] → медиана 300 мс; всего замеров 130.
		s, err := q.BaselineEndpointP95(ctx, projectID4, "GET /reg", 7, regNow)
		if err != nil {
			t.Fatalf("BaselineEndpointP95: %v", err)
		}
		if s.Samples != 130 {
			t.Fatalf("baseline samples = %d, want 130", s.Samples)
		}
		assertNearF(t, "baseline median p95 ms", s.Value, 300, 1)
	})

	t.Run("RecentEndpointP95Empty", func(t *testing.T) {
		// Нет данных → Samples 0 и Value 0 (не NaN пустого quantilesMerge).
		s, err := q.RecentEndpointP95(ctx, projectID4, "GET /nope", regRecentFrom, regRecentTo)
		if err != nil {
			t.Fatalf("RecentEndpointP95 empty: %v", err)
		}
		if s.Samples != 0 || s.Value != 0 {
			t.Fatalf("empty recent = %+v, want {0 0}", s)
		}
	})

	t.Run("RecentVitalP75", func(t *testing.T) {
		// Свежее окно: 30 замеров lcp 2000 → p75 = 2000 (уже в мс).
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
		// Дневные p75 = [2000, 500, 800] → медиана 800; всего замеров 70.
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

	t.Run("TopEndpointsByTraffic", func(t *testing.T) {
		// За 7 дней «GET /reg» (130) — самый нагруженный эндпойнт проекта.
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
		// Только страницы с замерами vital'ов: /vpage (70) перед /vpage2 (5);
		// «GET /reg» без measurements отфильтрован HAVING.
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

// TestRating проверяет пороги Google для рейтинга Web Vitals по p75, включая
// границы (good включительна) и неизвестное имя (→ ""). Docker не нужен.
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

// assertNearF — как assertNear, но для float-величин (p75 web vitals).
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
