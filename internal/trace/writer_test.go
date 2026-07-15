package trace_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

func TestSpanWriterInsertsTransactionAndSpansAndCloseFlushes(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	w := trace.NewSpanWriter(conn)
	go w.Run()

	start := time.Now().UTC().Truncate(time.Millisecond)
	tr := trace.Transaction{
		TraceID:     "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:      "00f067aa0ba902b7",
		Name:        "GET /api/users",
		Op:          "http.server",
		Status:      "ok",
		Start:       start,
		End:         start.Add(300 * time.Millisecond),
		Environment: "production",
		Release:     "v1.2.3",
		ServerName:  "web-1",
		UserID:      "u-42",
		Tags:        map[string]string{"region": "eu"},
		Source:      "sentry",
		Spans: []trace.Span{
			{SpanID: "s1", ParentSpanID: "00f067aa0ba902b7", Op: "db.query", Description: "SELECT 1",
				Start: start.Add(10 * time.Millisecond), End: start.Add(60 * time.Millisecond), Status: "ok"},
			{SpanID: "s2", ParentSpanID: "00f067aa0ba902b7", Op: "http.client", Description: "GET https://x/y",
				Start: start.Add(70 * time.Millisecond), End: start.Add(120 * time.Millisecond), Status: "ok",
				Data: map[string]any{"http.status_code": 200}},
			{SpanID: "s3", ParentSpanID: "s1", Op: "db.query", Description: "SELECT 2",
				Start: start.Add(130 * time.Millisecond), End: start.Add(140 * time.Millisecond), Status: "internal_error"},
		},
	}
	w.Add(777, tr)

	// Close без предшествующего тика/кика обязан слить остаток буфера.
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	var txCnt uint64
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM transactions WHERE project_id = 777").Scan(&txCnt); err != nil {
		t.Fatalf("count transactions: %v", err)
	}
	if txCnt != 1 {
		t.Fatalf("transactions count = %d, want 1", txCnt)
	}

	var spanCnt uint64
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM spans WHERE project_id = 777").Scan(&spanCnt); err != nil {
		t.Fatalf("count spans: %v", err)
	}
	if spanCnt != 4 { // 3 дочерних + корневой
		t.Fatalf("spans count = %d, want 4 (3 children + root)", spanCnt)
	}

	var (
		name, op, status, env, release, server, userID, source, spanID string
		ts                                                             time.Time
		durUS                                                          uint32
		tags                                                           map[string]string
	)
	if err := conn.QueryRow(ctx, `SELECT transaction, op, timestamp, duration_us, status,
		environment, release, server_name, user_id, tags, source, span_id
		FROM transactions WHERE project_id = 777`).Scan(
		&name, &op, &ts, &durUS, &status, &env, &release, &server, &userID, &tags, &source, &spanID); err != nil {
		t.Fatalf("select transaction: %v", err)
	}
	if name != "GET /api/users" || op != "http.server" || status != "ok" {
		t.Fatalf("transaction row: name=%q op=%q status=%q", name, op, status)
	}
	if durUS != 300_000 {
		t.Fatalf("transaction duration_us = %d, want 300000", durUS)
	}
	if !ts.Equal(start) {
		t.Fatalf("transaction timestamp = %s, want %s", ts, start)
	}
	if env != "production" || release != "v1.2.3" || server != "web-1" || userID != "u-42" ||
		source != "sentry" || spanID != "00f067aa0ba902b7" || tags["region"] != "eu" {
		t.Fatalf("transaction row meta: env=%q release=%q server=%q user=%q source=%q span=%q tags=%v",
			env, release, server, userID, source, spanID, tags)
	}

	// Корневой спан обязан быть и в spans — иначе waterfall без корня.
	var rootParent, rootTx, rootOp string
	var rootDur uint32
	if err := conn.QueryRow(ctx, `SELECT parent_span_id, transaction, op, duration_us
		FROM spans WHERE project_id = 777 AND span_id = '00f067aa0ba902b7'`).Scan(
		&rootParent, &rootTx, &rootOp, &rootDur); err != nil {
		t.Fatalf("select root span: %v", err)
	}
	if rootParent != "" || rootTx != "GET /api/users" || rootOp != "http.server" || rootDur != 300_000 {
		t.Fatalf("root span: parent=%q tx=%q op=%q dur=%d", rootParent, rootTx, rootOp, rootDur)
	}

	var (
		childDesc, childData, childTx, childEnv, childStatus string
		childHash                                            uint64
		childDur                                             uint32
		childTS                                              time.Time
	)
	if err := conn.QueryRow(ctx, `SELECT description, description_hash, duration_us, timestamp,
		data, transaction, environment, status
		FROM spans WHERE project_id = 777 AND span_id = 's2'`).Scan(
		&childDesc, &childHash, &childDur, &childTS, &childData, &childTx, &childEnv, &childStatus); err != nil {
		t.Fatalf("select child span: %v", err)
	}
	if childDesc != "GET https://x/y" || childDur != 50_000 || childStatus != "ok" {
		t.Fatalf("child span: desc=%q dur=%d status=%q", childDesc, childDur, childStatus)
	}
	if childHash != trace.DescriptionHash("http.client", "GET https://x/y") {
		t.Fatalf("child span description_hash = %d, want %d",
			childHash, trace.DescriptionHash("http.client", "GET https://x/y"))
	}
	if !childTS.Equal(start.Add(70 * time.Millisecond)) {
		t.Fatalf("child span timestamp = %s, want %s", childTS, start.Add(70*time.Millisecond))
	}
	if childData != `{"http.status_code":200}` {
		t.Fatalf("child span data = %q", childData)
	}
	// Спаны наследуют transaction/environment транзакции — иначе фильтры по ним слепы.
	if childTx != "GET /api/users" || childEnv != "production" {
		t.Fatalf("child span inherits: tx=%q env=%q", childTx, childEnv)
	}

	// Материализованная вьюха наполняется автоматически при вставке.
	var mvCnt uint64
	if err := conn.QueryRow(ctx, `SELECT countMerge(cnt) FROM transactions_5m
		WHERE project_id = 777 AND transaction = 'GET /api/users'`).Scan(&mvCnt); err != nil {
		t.Fatalf("select mv: %v", err)
	}
	if mvCnt != 1 {
		t.Fatalf("transactions_5m count = %d, want 1", mvCnt)
	}
}
